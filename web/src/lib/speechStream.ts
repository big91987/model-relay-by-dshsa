import { ApiError } from './api'
import type { SpeechClip, SpeechInput } from './types'

/**
 * The relay sends PCM as server-sent events so playback can start while MiniMax
 * is still synthesizing. This player turns each audio event into a Web Audio
 * buffer and schedules it after the previous one; stopping aborts both the
 * download and every scheduled source.
 */

export interface SpeechStreamProgress {
  /** Seconds of audio received from the relay so far. */
  generated: number
  /** Seconds of that audio the browser has played so far. */
  played: number
}

export interface SpeechStreamHandlers {
  onClip?: (clip: SpeechClip) => void
  onProgress?: (progress: SpeechStreamProgress) => void
  onPlaybackEnd?: () => void
}

interface StreamMeta {
  format?: string
  sample_rate?: number
  channels?: number
}

interface StreamBlock {
  event: string
  data: string
}

const DEFAULT_SAMPLE_RATE = 32000
const FIRST_CHUNK_LEAD_SECONDS = 0.15
const NEXT_CHUNK_LEAD_SECONDS = 0.05
const PROGRESS_INTERVAL_MS = 200

export class SpeechStreamPlayer {
  private controller: AbortController | null = null
  private context: AudioContext | null = null
  private gain: GainNode | null = null
  private sources: AudioBufferSourceNode[] = []
  private nextStart = 0
  private firstStart = 0
  private sampleRate = DEFAULT_SAMPLE_RATE
  private leftover = new Uint8Array(0)
  private received = 0
  private streamEnded = false
  private clipReceived = false
  private finished = false
  private stopRequested = false
  private disposed = false
  private timer: number | null = null
  private handlers: SpeechStreamHandlers = {}

  get isStopped(): boolean {
    return this.stopRequested
  }

  async start(input: SpeechInput, handlers: SpeechStreamHandlers): Promise<void> {
    this.handlers = handlers
    this.controller = new AbortController()
    const context = new AudioContext()
    this.context = context
    this.gain = context.createGain()
    this.gain.connect(context.destination)
    // The caller opens the player from the submit gesture; resuming here keeps
    // autoplay policies satisfied in browsers that start suspended.
    if (context.state === 'suspended') {
      try {
        await context.resume()
      } catch {
        // The first chunk retries the resume call.
      }
    }
    this.timer = window.setInterval(() => this.tick(), PROGRESS_INTERVAL_MS)

    try {
      const response = await fetch('/api/speech/stream', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(input),
        credentials: 'same-origin',
        signal: this.controller.signal,
      })
      if (!response.ok) {
        throw await speechRequestError(response)
      }
      if (!response.body) {
        throw new Error('This browser cannot read the streaming speech response')
      }
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        buffer = this.consumeBlocks(buffer)
      }
      buffer += decoder.decode()
      if (buffer.trim()) {
        const block = parseSpeechBlock(buffer)
        if (block) this.handleBlock(block)
      }
      this.streamEnded = true
      if (!this.clipReceived) {
        throw new Error('The speech stream ended before the clip was ready')
      }
      this.tick()
    } catch (err) {
      if (this.stopRequested || this.disposed) return
      this.dispose()
      throw err
    }
  }

  stop(): void {
    if (this.stopRequested) return
    this.stopRequested = true
    this.controller?.abort()
    this.stopSources()
    this.clearTimer()
    const context = this.context
    this.context = null
    if (context && context.state !== 'closed') {
      void context.close().catch(() => {})
    }
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    this.controller?.abort()
    this.stopSources()
    this.clearTimer()
    const context = this.context
    this.context = null
    if (context && context.state !== 'closed') {
      void context.close().catch(() => {})
    }
  }

  private consumeBlocks(buffer: string): string {
    let rest = buffer
    let boundary = rest.indexOf('\n\n')
    while (boundary >= 0) {
      const block = parseSpeechBlock(rest.slice(0, boundary))
      rest = rest.slice(boundary + 2)
      if (block) this.handleBlock(block)
      boundary = rest.indexOf('\n\n')
    }
    return rest
  }

  private handleBlock(block: StreamBlock): void {
    let payload: unknown
    try {
      payload = block.data ? JSON.parse(block.data) : null
    } catch {
      throw new Error('The speech stream could not be decoded')
    }
    switch (block.event) {
      case 'meta': {
        const meta = payload as StreamMeta | null
        if (meta && typeof meta.sample_rate === 'number' && meta.sample_rate > 0) {
          this.sampleRate = meta.sample_rate
        }
        break
      }
      case 'audio': {
        const audio = (payload as { audio?: unknown } | null)?.audio
        if (typeof audio === 'string' && audio) {
          this.enqueue(base64Bytes(audio))
        }
        break
      }
      case 'done': {
        const clip = (payload as { clip?: SpeechClip } | null)?.clip
        this.streamEnded = true
        if (clip) {
          this.clipReceived = true
          this.handlers.onClip?.(clip)
        }
        break
      }
      case 'error': {
        const message = (payload as { error?: unknown } | null)?.error
        throw new Error(typeof message === 'string' && message ? message : 'Speech generation failed')
      }
      default:
        break
    }
  }

  private enqueue(bytes: Uint8Array): void {
    const context = this.context
    if (!context) return
    let combined = bytes
    if (this.leftover.length > 0) {
      combined = new Uint8Array(this.leftover.length + bytes.length)
      combined.set(this.leftover)
      combined.set(bytes, this.leftover.length)
    }
    const usable = combined.length - (combined.length % 2)
    this.leftover = usable < combined.length ? combined.slice(usable) : new Uint8Array(0)
    if (usable === 0) return

    if (context.state === 'suspended') {
      void context.resume()
    }
    const samples = usable / 2
    const buffer = context.createBuffer(1, samples, this.sampleRate)
    const channel = buffer.getChannelData(0)
    const view = new DataView(combined.buffer, combined.byteOffset, usable)
    for (let i = 0; i < samples; i += 1) {
      channel[i] = view.getInt16(i * 2, true) / 32768
    }

    const source = context.createBufferSource()
    source.buffer = buffer
    source.connect(this.gain ?? context.destination)
    const lead = this.nextStart > 0 ? NEXT_CHUNK_LEAD_SECONDS : FIRST_CHUNK_LEAD_SECONDS
    const startAt = Math.max(this.nextStart, context.currentTime + lead)
    this.nextStart = startAt + buffer.duration
    if (this.firstStart === 0) this.firstStart = startAt
    source.onended = () => {
      this.sources = this.sources.filter((item) => item !== source)
      if (this.streamEnded && this.sources.length === 0) this.finishPlayback()
    }
    source.start(startAt)
    this.sources.push(source)
    this.received += buffer.duration
    this.tick()
  }

  private tick(): void {
    const context = this.context
    if (!context || this.finished) return
    const played =
      this.firstStart === 0 ? 0 : Math.min(this.received, Math.max(0, context.currentTime - this.firstStart))
    this.handlers.onProgress?.({ generated: this.received, played })
    if (this.streamEnded && this.received > 0 && this.sources.length === 0) {
      this.finishPlayback()
    }
  }

  private finishPlayback(): void {
    if (this.finished) return
    this.finished = true
    this.clearTimer()
    const context = this.context
    this.context = null
    if (context && context.state !== 'closed') {
      void context.close().catch(() => {})
    }
    this.handlers.onPlaybackEnd?.()
  }

  private stopSources(): void {
    for (const source of this.sources) {
      try {
        source.stop()
      } catch {
        // A source that already ended cannot be stopped again.
      }
      source.disconnect()
    }
    this.sources = []
  }

  private clearTimer(): void {
    if (this.timer !== null) {
      window.clearInterval(this.timer)
      this.timer = null
    }
  }
}

function parseSpeechBlock(raw: string): StreamBlock | null {
  let event = 'message'
  const data: string[] = []
  for (const rawLine of raw.split('\n')) {
    const line = rawLine.replace(/\r$/, '')
    if (line.startsWith('event:')) {
      event = line.slice(6).trim()
    } else if (line.startsWith('data:')) {
      data.push(line.slice(5).trim())
    }
  }
  if (data.length === 0) return null
  return { event, data: data.join('\n') }
}

function base64Bytes(data: string): Uint8Array {
  const binary = atob(data)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes
}

async function speechRequestError(response: Response): Promise<ApiError> {
  let message = `Request failed with status ${response.status}`
  try {
    const text = await response.text()
    if (text) {
      const data = JSON.parse(text) as { error?: unknown }
      if (typeof data.error === 'string' && data.error) {
        message = data.error
      }
    }
  } catch {
    // Keep the status message when the body is not JSON.
  }
  if (response.status === 401) {
    window.dispatchEvent(new Event('mr:unauthorized'))
  }
  return new ApiError(message, response.status)
}
