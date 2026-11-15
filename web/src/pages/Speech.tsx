import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Modal } from '../components/Modal'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { relativeTime } from '../lib/format'
import { SpeechStreamPlayer, type SpeechStreamProgress } from '../lib/speechStream'
import { useToast } from '../lib/toast'
import type { ModelInfo, SpeechClip, SpeechClipStatus, SpeechVoice, VoiceClone } from '../lib/types'
import { t } from '../lib/i18n'

const MAX_TEXT = 50000
// Text longer than this is accepted as a background job instead of holding the
// page open. Keep in sync with speechSyncLimit in internal/gateway/tts.go.
const SYNC_TEXT_LIMIT = 2000
const MAX_PREVIEW_TEXT = 1000
const MAX_SAMPLE_BYTES = 20 * 1024 * 1024
const MIN_SAMPLE_SECONDS = 10
const MAX_SAMPLE_SECONDS = 5 * 60
const DEFAULT_SPEECH_MODEL = 'speech-2.8-hd'
const DEFAULT_VOICE_ID = 'English_expressive_narrator'
const DEFAULT_CLONE_PREVIEW =
  'Hi, this is a preview of my cloned voice. I can now use it to read any new text aloud.'
const SAMPLE_EXTENSIONS = ['mp3', 'm4a', 'wav']

function isSpeechModel(model: ModelInfo): boolean {
  if (!model.types.includes('minimax')) return false
  return /^speech-/i.test(model.id) || /^minimax-speech/i.test(model.id)
}

function isPending(clip: SpeechClip): boolean {
  return clip.status === 'queued' || clip.status === 'running'
}

function formatDuration(durationMS: number): string {
  if (!durationMS || durationMS <= 0) return t("Unknown length")
  const seconds = Math.round(durationMS / 1000)
  const minutes = Math.floor(seconds / 60)
  return `${minutes}:${String(seconds % 60).padStart(2, '0')}`
}

function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function formatClock(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0:00'
  const whole = Math.floor(seconds)
  return `${Math.floor(whole / 60)}:${String(whole % 60).padStart(2, '0')}`
}

function fileExtension(name: string): string {
  const index = name.lastIndexOf('.')
  return index >= 0 ? name.slice(index + 1).toLowerCase() : ''
}

function pickRecorderMimeType(): string {
  const candidates = ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4', 'audio/ogg;codecs=opus']
  for (const candidate of candidates) {
    if (typeof MediaRecorder !== 'undefined' && MediaRecorder.isTypeSupported(candidate)) {
      return candidate
    }
  }
  return ''
}

function encodePcm16Wav(samples: Float32Array, sampleRate: number): Blob {
  const buffer = new ArrayBuffer(44 + samples.length * 2)
  const view = new DataView(buffer)
  const writeString = (offset: number, value: string) => {
    for (let index = 0; index < value.length; index += 1) {
      view.setUint8(offset + index, value.charCodeAt(index))
    }
  }
  writeString(0, 'RIFF')
  view.setUint32(4, 36 + samples.length * 2, true)
  writeString(8, 'WAVE')
  writeString(12, 'fmt ')
  view.setUint32(16, 16, true)
  view.setUint16(20, 1, true)
  view.setUint16(22, 1, true)
  view.setUint32(24, sampleRate, true)
  view.setUint32(28, sampleRate * 2, true)
  view.setUint16(32, 2, true)
  view.setUint16(34, 16, true)
  writeString(36, 'data')
  view.setUint32(40, samples.length * 2, true)
  for (let index = 0; index < samples.length; index += 1) {
    const sample = Math.max(-1, Math.min(1, samples[index] ?? 0))
    view.setInt16(44 + index * 2, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true)
  }
  return new Blob([buffer], { type: 'audio/wav' })
}

// MiniMax only accepts mp3, m4a and wav. MediaRecorder produces webm/opus in
// most browsers, so the recording is decoded, downmixed to mono and resampled
// to 16 kHz WAV locally before it is uploaded.
async function recordingToWav(blob: Blob, context: AudioContext): Promise<{ blob: Blob; duration: number }> {
  const decoded = await context.decodeAudioData(await blob.arrayBuffer())
  const sampleRate = 16000
  const frames = Math.max(1, Math.ceil(decoded.duration * sampleRate))
  const offline = new OfflineAudioContext(1, frames, sampleRate)
  const source = offline.createBufferSource()
  source.buffer = decoded
  source.connect(offline.destination)
  source.start(0)
  const rendered = await offline.startRendering()
  return { blob: encodePcm16Wav(rendered.getChannelData(0), sampleRate), duration: decoded.duration }
}

function statusLabel(status: SpeechClipStatus): string {
  switch (status) {
    case 'queued':
      return t("Queued")
    case 'running':
      return t("Synthesizing")
    case 'succeeded':
      return t("Ready")
    case 'failed':
      return t("Failed")
  }
}

function SpeechStatus({ status }: { status: SpeechClipStatus }) {
  const tone = status === 'succeeded' ? 'success' : status === 'failed' ? 'danger' : 'muted'
  return (
    <span className={`badge badge-${tone}`}>
      <span className={`badge-dot badge-dot-${tone}`} />
      {statusLabel(status)}
    </span>
  )
}

function progressPercent(clip: SpeechClip): number {
  if (clip.status === 'succeeded') return 100
  if (!clip.total_chunks) return 0
  return Math.min(100, Math.round((clip.done_chunks / clip.total_chunks) * 100))
}

function SpeechCard({ clip }: { clip: SpeechClip }) {
  const ready = clip.status === 'succeeded' && clip.audio_ready && clip.content_url
  const format = (clip.format || 'mp3').toUpperCase()
  const size = formatBytes(clip.size_bytes)
  const percent = progressPercent(clip)
  const oneTask = clip.async
  return (
    <article className="speech-card">
      <div className="speech-card-head">
        <div>
          <div className="speech-voice">{clip.voice_name || clip.voice_id}</div>
          <div className="video-meta">
            {clip.model} ·{' '}
            {clip.status === 'succeeded' ? `${formatDuration(clip.duration_ms)} · ` : ''}
            {t('{n} characters', { n: clip.characters.toLocaleString() })}{size ? ` · ${size}` : ''} · {relativeTime(clip.created_at)}
          </div>
        </div>
        <SpeechStatus status={clip.status} />
      </div>
      <p className="speech-text">{clip.text}</p>
      {isPending(clip) ? (
        <div className="speech-task-state">
          <div className="speech-task-line">
            <Spinner size={14} />
            <span className="speech-task-label">
              {clip.status === 'queued'
                ? t('Submitting the task…')
                : oneTask
                  ? t('MiniMax is generating the full audio as one task…')
                  : t('Synthesizing part {current} of {total}…', {
                      current: Math.min(clip.done_chunks + 1, clip.total_chunks),
                      total: clip.total_chunks,
                    })}
            </span>
            {oneTask ? null : <span className="speech-task-percent">{percent}%</span>}
          </div>
          {oneTask ? (
            <div
              className="speech-progress speech-progress-indeterminate"
              role="progressbar"
              aria-valuetext={t('Waiting for MiniMax to finish the full audio')}
              aria-label={t("Speech synthesis progress")}
            >
              <span className="speech-progress-bar" />
            </div>
          ) : (
            <div
              className="speech-progress"
              role="progressbar"
              aria-valuenow={percent}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-label={t("Speech synthesis progress")}
            >
              <span className="speech-progress-bar" style={{ width: `${percent}%` }} />
            </div>
          )}
          <span className="speech-task-note">
            {oneTask
              ? t('This is one MiniMax long-text task, tracked by its task id. Refresh, reopen the page or restart the gateway and it keeps following the same task.')
              : t("This task keeps running in the background; refresh or leave the page and come back to download it.")}
          </span>
        </div>
      ) : clip.status === 'failed' ? (
        <div className="speech-audio-missing">
          <Icon name="alert" size={14} /> {clip.error || t("Speech synthesis failed.")}
        </div>
      ) : ready ? (
        <audio className="speech-audio" controls preload="metadata" src={clip.content_url}>
          {t("Your browser cannot play this audio.")}
        </audio>
      ) : (
        <div className="speech-audio-missing">
          <Icon name="alert" size={14} /> {t("The stored audio file is not available.")}
        </div>
      )}
      <div className="video-actions">
        {ready ? (
          <a className="btn btn-sm btn-ghost" href={`${clip.content_url}?download=1`}>
            <Icon name="download" size={15} /> {t('Download {format}', { format })}
          </a>
        ) : (
          <span className="video-meta">
            {clip.status === 'failed'
              ? t("No audio to download")
              : oneTask
                ? t('One task will produce the full audio')
                : t('Audio will be ready after {parts} parts', { parts: clip.total_chunks })}
          </span>
        )}
        <span className="video-meta">{t('by {channel}', { channel: clip.channel_name })}</span>
      </div>
      <div className="speech-voice-id mono">{clip.voice_id}</div>
    </article>
  )
}

function CloneCard({
  clone,
  selected,
  previewing,
  previewSrc,
  onUse,
  onRegenerate,
}: {
  clone: VoiceClone
  selected: boolean
  previewing: boolean
  previewSrc: string
  onUse: () => void
  onRegenerate: () => void
}) {
  const ready = clone.preview_ready && previewSrc
  const sampleSize = formatBytes(clone.sample_bytes)
  return (
    <article className={`voice-clone-card${selected ? ' voice-clone-card-selected' : ''}`}>
      <div className="voice-clone-head">
        <div className="voice-clone-name">{clone.name || clone.voice_id}</div>
        {selected ? (
          <span className="badge badge-success">
            <Icon name="check" size={12} /> {t("Selected")}
          </span>
        ) : (
          <span className="badge badge-muted">{t("Cloned")}</span>
        )}
      </div>
      <div className="video-meta voice-clone-meta">
        {clone.model} · {clone.sample_format.toUpperCase()}
        {sampleSize ? ` · ${sampleSize}` : ''} · {relativeTime(clone.created_at)}
      </div>
      <p className="speech-text">{clone.preview_text}</p>
      {ready ? (
        <audio className="speech-audio" controls preload="metadata" src={previewSrc}>
          {t("Your browser cannot play this preview.")}
        </audio>
      ) : (
        <div className="speech-note">
          <Icon name="alert" size={14} />
          <span>
            {t("The preview is not ready yet.")}
            <button className="link-button" onClick={onRegenerate} disabled={previewing}>
              {previewing ? t("Generating…") : t("Generate preview")}
            </button>
          </span>
        </div>
      )}
      <div className="video-actions">
        <button className="btn btn-sm btn-ghost" onClick={onUse} disabled={selected}>
          {selected ? <Icon name="check" size={15} /> : <Icon name="speech" size={15} />}
          {selected ? t("In use") : t("Use voice")}
        </button>
        {ready ? (
          <button className="btn btn-sm btn-ghost" onClick={onRegenerate} disabled={previewing}>
            <Icon name="refresh" size={14} /> {previewing ? t("Generating…") : t("New preview")}
          </button>
        ) : null}
      </div>
      <div className="speech-voice-id mono" title={clone.source_filename}>
        {clone.voice_id} · by {clone.channel_name}
      </div>
    </article>
  )
}

export default function Speech() {
  const toast = useToast()
  const [models, setModels] = useState<ModelInfo[]>([])
  const [clips, setClips] = useState<SpeechClip[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [live, setLive] = useState(false)
  const [phase, setPhase] = useState<'streaming' | 'playing'>('streaming')
  const [progress, setProgress] = useState<SpeechStreamProgress>({ generated: 0, played: 0 })
  const [streamNotice, setStreamNotice] = useState('')
  const playerRef = useRef<SpeechStreamPlayer | null>(null)
  const clipSavedRef = useRef(false)

  const [model, setModel] = useState('')
  const [text, setText] = useState('')
  const [speed, setSpeed] = useState(1)
  const [voices, setVoices] = useState<SpeechVoice[]>([])
  const [voiceId, setVoiceId] = useState('')
  const [voicesLoading, setVoicesLoading] = useState(false)
  const [voiceWarning, setVoiceWarning] = useState('')
  const [voiceError, setVoiceError] = useState('')
  const [voiceNonce, setVoiceNonce] = useState(0)
  const [clones, setClones] = useState<VoiceClone[]>([])
  const [previewNonces, setPreviewNonces] = useState<Record<number, number>>({})
  const [previewingCloneId, setPreviewingCloneId] = useState<number | null>(null)

  const [cloneOpen, setCloneOpen] = useState(false)
  const [cloneName, setCloneName] = useState('')
  const [cloneFile, setCloneFile] = useState<File | null>(null)
  const [cloneSampleDuration, setCloneSampleDuration] = useState<number | null>(null)
  const [clonePreviewText, setClonePreviewText] = useState(DEFAULT_CLONE_PREVIEW)
  const [cloneModel, setCloneModel] = useState('')
  const [cloneAuthorized, setCloneAuthorized] = useState(false)
  const [cloneSubmitting, setCloneSubmitting] = useState(false)
  const [cloneError, setCloneError] = useState('')
  const [cloneSource, setCloneSource] = useState<'upload' | 'record'>('upload')
  const [recording, setRecording] = useState(false)
  const [recordingSeconds, setRecordingSeconds] = useState(0)
  const [recordingUrl, setRecordingUrl] = useState('')
  const [recordingError, setRecordingError] = useState('')
  const [recordingApplied, setRecordingApplied] = useState(false)
  const [preparingRecording, setPreparingRecording] = useState(false)

  const recorderRef = useRef<MediaRecorder | null>(null)
  const streamRef = useRef<MediaStream | null>(null)
  const timerRef = useRef<number | null>(null)
  const startedAtRef = useRef(0)
  const audioContextRef = useRef<AudioContext | null>(null)
  const discardRecordingRef = useRef(false)
  const recordingStartingRef = useRef(false)

  const recordingSupported =
    typeof window !== 'undefined' &&
    typeof window.MediaRecorder !== 'undefined' &&
    !!navigator.mediaDevices?.getUserMedia

  const speechModels = useMemo(() => models.filter(isSpeechModel), [models])
  const selectedVoice = useMemo(() => voices.find((voice) => voice.voice_id === voiceId), [voices, voiceId])
  const selectedClone = useMemo(
    () => clones.find((clone) => clone.voice_id === voiceId),
    [clones, voiceId],
  )
  const pending = clips.some(isPending)
  const backgroundTask = text.trim().length > SYNC_TEXT_LIMIT
  const voiceGroups = useMemo(() => {
    const groups = new Map<string, SpeechVoice[]>()
    for (const voice of voices) {
      const language = voice.language || t('Other')
      const list = groups.get(language) ?? []
      list.push(voice)
      groups.set(language, list)
    }
    return Array.from(groups.entries()).sort(([a], [b]) => a.localeCompare(b))
  }, [voices])

  const previewSrc = useCallback(
    (clone: VoiceClone) =>
      clone.preview_url ? `${clone.preview_url}?v=${previewNonces[clone.id] ?? 0}` : '',
    [previewNonces],
  )

  const voiceHint = voicesLoading
    ? t("Loading system voices from MiniMax…")
    : selectedClone
      ? t('Cloned from {file} on {channel}. Listen to the preview, then generate speech with it.', {
            file: selectedClone.source_filename,
            channel: selectedClone.channel_name,
          })
      : selectedVoice
        ? `${selectedVoice.description ? `${selectedVoice.description} ` : ''}${t('ID: {id}', { id: selectedVoice.voice_id })}`
        : clones.length > 0
          ? t("Choose a system voice or one of your cloned voices.")
          : t("No system voices are available.")
  const canClone = speechModels.length > 0 && !loading

  const cleanupRecording = useCallback(() => {
    discardRecordingRef.current = true
    if (timerRef.current !== null) {
      window.clearInterval(timerRef.current)
      timerRef.current = null
    }
    const recorder = recorderRef.current
    recorderRef.current = null
    if (recorder && recorder.state !== 'inactive') {
      try {
        recorder.stop()
      } catch {
        // The recorder can already be stopping; stopping the tracks is enough.
      }
    }
    streamRef.current?.getTracks().forEach((track) => track.stop())
    streamRef.current = null
    if (audioContextRef.current) {
      void audioContextRef.current.close().catch(() => {})
      audioContextRef.current = null
    }
    setRecording(false)
    setRecordingSeconds(0)
    setRecordingApplied(false)
    setPreparingRecording(false)
    setRecordingUrl((previous) => {
      if (previous) URL.revokeObjectURL(previous)
      return ''
    })
  }, [])

  useEffect(() => {
    if (!cloneOpen) cleanupRecording()
  }, [cloneOpen, cleanupRecording])

  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const [modelResponse, speechResponse, cloneResponse] = await Promise.all([
          api.models(),
          api.speech(),
          api.voiceClones(),
        ])
        if (!active) return
        setModels(modelResponse.data)
        setClips(speechResponse.items)
        setTotal(speechResponse.total)
        setClones(cloneResponse.items)
      } catch (err) {
        if (active) setError(err instanceof Error ? err.message : t("Failed to load text to speech"))
      } finally {
        if (active) setLoading(false)
      }
    })()
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    setModel((previous) => {
      if (previous && speechModels.some((item) => item.id === previous)) return previous
      if (speechModels.some((item) => item.id === DEFAULT_SPEECH_MODEL)) return DEFAULT_SPEECH_MODEL
      return speechModels[0]?.id ?? ''
    })
  }, [speechModels])

  useEffect(() => {
    if (!model) {
      setVoices([])
      setVoiceId('')
      setVoiceWarning('')
      setVoiceError('')
      return
    }
    let active = true
    setVoicesLoading(true)
    setVoiceError('')
    api
      .speechVoices(model)
      .then((catalog) => {
        if (!active) return
        setVoices(catalog.voices)
        setVoiceWarning(catalog.warning ?? '')
        setVoiceId((previous) => {
          if (clones.some((clone) => clone.voice_id === previous)) return previous
          if (catalog.voices.some((voice) => voice.voice_id === previous)) return previous
          const preferred = catalog.voices.find((voice) => voice.voice_id === DEFAULT_VOICE_ID)
          return preferred?.voice_id ?? catalog.voices[0]?.voice_id ?? clones[0]?.voice_id ?? ''
        })
      })
      .catch((err: unknown) => {
        if (!active) return
        setVoices([])
        setVoiceWarning('')
        setVoiceError(err instanceof Error ? err.message : t("Failed to load system voices"))
        setVoiceId((previous) => clones.find((clone) => clone.voice_id === previous)?.voice_id ?? '')
      })
      .finally(() => {
        if (active) setVoicesLoading(false)
      })
    return () => {
      active = false
    }
    // clones are read only to preserve a cloned selection across voice reloads.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [model, voiceNonce])

  useEffect(() => {
    return () => {
      playerRef.current?.dispose()
      playerRef.current = null
    }
  }, [])

  const refreshClips = useCallback(async () => {
    try {
      const response = await api.speech()
      setClips(response.items)
      setTotal(response.total)
    } catch {
      // Polling and manual refreshes are best effort; the visible history stays as-is.
    }
  }, [])

  const refreshClones = useCallback(async () => {
    try {
      const response = await api.voiceClones()
      setClones(response.items)
    } catch {
      // Refreshing is best effort; the visible cloned voices stay as-is.
    }
  }, [])

  // Background jobs advance server-side. Poll only while one is queued or
  // running, so a refreshed page picks up progress and the finished audio.
  useEffect(() => {
    if (!pending) return
    const timer = window.setInterval(() => {
      void refreshClips()
    }, 3000)
    return () => window.clearInterval(timer)
  }, [pending, refreshClips])

  const refresh = () => {
    void refreshClips()
    void refreshClones()
    setVoiceNonce((previous) => previous + 1)
  }

  const openCloneModal = () => {
    cleanupRecording()
    setCloneSource('upload')
    setCloneName('')
    setCloneFile(null)
    setCloneSampleDuration(null)
    setClonePreviewText(DEFAULT_CLONE_PREVIEW)
    setCloneModel(model || speechModels[0]?.id || '')
    setCloneAuthorized(false)
    setCloneError('')
    setRecordingError('')
    setCloneOpen(true)
  }

  const chooseVoice = (id: string) => {
    setVoiceId(id)
    const clone = clones.find((item) => item.voice_id === id)
    if (clone && clone.model && speechModels.some((item) => item.id === clone.model)) {
      setModel(clone.model)
    }
  }

  const pickCloneFile = (file: File | null) => {
    setCloneFile(file)
    setCloneSampleDuration(null)
    setCloneError('')
    if (!file) return
    if (!SAMPLE_EXTENSIONS.includes(fileExtension(file.name))) {
      setCloneError(t("Choose an mp3, m4a or wav recording."))
      return
    }
    if (file.size > MAX_SAMPLE_BYTES) {
      setCloneError(t("The recording must be 20 MB or smaller."))
      return
    }
    const url = URL.createObjectURL(file)
    const audio = new Audio()
    audio.preload = 'metadata'
    audio.onloadedmetadata = () => {
      URL.revokeObjectURL(url)
      if (Number.isFinite(audio.duration) && audio.duration > 0) {
        setCloneSampleDuration(audio.duration)
      }
    }
    audio.onerror = () => URL.revokeObjectURL(url)
    audio.src = url
  }

  const applyRecording = async (blob: Blob) => {
    setPreparingRecording(true)
    setRecordingError('')
    try {
      const context = audioContextRef.current ?? new AudioContext()
      audioContextRef.current = context
      const prepared = await recordingToWav(blob, context)
      if (discardRecordingRef.current) return
      if (prepared.duration < MIN_SAMPLE_SECONDS) {
        setRecordingError(
          t('The recording is {seconds} seconds; MiniMax needs at least {min}. Record again.', {
            seconds: prepared.duration.toFixed(1),
            min: MIN_SAMPLE_SECONDS,
          }),
        )
        setRecordingApplied(false)
        setCloneFile(null)
        setCloneSampleDuration(null)
        return
      }
      if (prepared.duration > MAX_SAMPLE_SECONDS) {
        setRecordingError(t('The recording is longer than {minutes} minutes. Record again.', { minutes: MAX_SAMPLE_SECONDS / 60 }))
        setRecordingApplied(false)
        setCloneFile(null)
        setCloneSampleDuration(null)
        return
      }
      const file = new File([prepared.blob], `voice-recording-${Date.now()}.wav`, { type: 'audio/wav' })
      setCloneFile(file)
      setCloneSampleDuration(prepared.duration)
      setRecordingApplied(true)
    } catch (err) {
      if (!discardRecordingRef.current) {
        setRecordingError(
          err instanceof Error ? t('Could not prepare the recording: {message}', { message: err.message }) : t("Could not prepare the recording."),
        )
      }
    } finally {
      setPreparingRecording(false)
    }
  }

  const startRecording = async () => {
    if (!recordingSupported) {
      setRecordingError(t("This browser cannot record audio. Upload a file instead."))
      return
    }
    if (recordingStartingRef.current) return
    recordingStartingRef.current = true
    cleanupRecording()
    setCloneFile(null)
    setCloneSampleDuration(null)
    setCloneError('')
    setRecordingError('')
    discardRecordingRef.current = false
    try {
      audioContextRef.current = new AudioContext()
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      if (discardRecordingRef.current) {
        stream.getTracks().forEach((track) => track.stop())
        if (audioContextRef.current) {
          void audioContextRef.current.close().catch(() => {})
          audioContextRef.current = null
        }
        return
      }
      const mimeType = pickRecorderMimeType()
      const recorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream)
      const chunks: Blob[] = []
      recorderRef.current = recorder
      streamRef.current = stream
      startedAtRef.current = Date.now()
      setRecording(true)
      setRecordingSeconds(0)
      recorder.ondataavailable = (event) => {
        if (event.data.size > 0) chunks.push(event.data)
      }
      recorder.onstop = () => {
        if (timerRef.current !== null) {
          window.clearInterval(timerRef.current)
          timerRef.current = null
        }
        stream.getTracks().forEach((track) => track.stop())
        if (streamRef.current === stream) streamRef.current = null
        if (recorderRef.current === recorder) recorderRef.current = null
        const seconds = Math.min((Date.now() - startedAtRef.current) / 1000, MAX_SAMPLE_SECONDS)
        setRecording(false)
        setRecordingSeconds(seconds)
        if (discardRecordingRef.current) return
        const blob = new Blob(chunks, { type: recorder.mimeType || 'audio/webm' })
        if (blob.size === 0) {
          setRecordingError(t("No audio was captured. Check your microphone and try again."))
          return
        }
        const url = URL.createObjectURL(blob)
        setRecordingUrl((previous) => {
          if (previous) URL.revokeObjectURL(previous)
          return url
        })
        void applyRecording(blob)
      }
      timerRef.current = window.setInterval(() => {
        const elapsed = (Date.now() - startedAtRef.current) / 1000
        setRecordingSeconds(Math.min(elapsed, MAX_SAMPLE_SECONDS))
        if (elapsed >= MAX_SAMPLE_SECONDS && recorderRef.current?.state === 'recording') {
          recorderRef.current.stop()
        }
      }, 200)
      recorder.start()
    } catch (err) {
      streamRef.current?.getTracks().forEach((track) => track.stop())
      streamRef.current = null
      recorderRef.current = null
      if (audioContextRef.current) {
        void audioContextRef.current.close().catch(() => {})
        audioContextRef.current = null
      }
      setRecording(false)
      setRecordingError(
        err instanceof Error ? t('Could not access the microphone: {message}', { message: err.message }) : t("Could not access the microphone."),
      )
    } finally {
      recordingStartingRef.current = false
    }
  }

  const stopRecording = () => {
    const recorder = recorderRef.current
    if (!recorder || recorder.state !== 'recording') return
    const elapsed = (Date.now() - startedAtRef.current) / 1000
    // MediaRecorder can drop the first frames, so wait a moment when the user
    // stops right at the 10-second boundary and verify the decoded length later.
    const remaining = MIN_SAMPLE_SECONDS + 0.6 - elapsed
    if (remaining > 0) {
      window.setTimeout(() => {
        if (recorderRef.current?.state === 'recording') recorderRef.current.stop()
      }, Math.ceil(remaining * 1000))
    } else {
      recorder.stop()
    }
  }

  const clearRecording = () => {
    cleanupRecording()
    setCloneFile(null)
    setCloneSampleDuration(null)
    setRecordingError('')
  }

  const switchCloneSource = (next: 'upload' | 'record') => {
    if (next === cloneSource) return
    cleanupRecording()
    setCloneSource(next)
    setCloneFile(null)
    setCloneSampleDuration(null)
    setCloneError('')
    setRecordingError('')
  }

  const submitClone = async () => {
    if (!cloneName.trim()) {
      setCloneError(t("Give the cloned voice a name."))
      return
    }
    if (!cloneFile) {
      setCloneError(
        cloneSource === 'record' ? t("Record a sample or switch to uploading a file.") : t("Choose the sample recording to clone."),
      )
      return
    }
    if (!SAMPLE_EXTENSIONS.includes(fileExtension(cloneFile.name))) {
      setCloneError(t("Choose an mp3, m4a or wav recording."))
      return
    }
    if (cloneFile.size > MAX_SAMPLE_BYTES) {
      setCloneError(t("The recording must be 20 MB or smaller."))
      return
    }
    if (!clonePreviewText.trim()) {
      setCloneError(t("Enter the text to preview with the cloned voice."))
      return
    }
    if (!cloneModel) {
      setCloneError(t("Choose a speech model for the preview."))
      return
    }
    if (!cloneAuthorized) {
      setCloneError(t("Confirm that you have permission to use this recording."))
      return
    }
    if (
      cloneSampleDuration !== null &&
      (cloneSampleDuration < MIN_SAMPLE_SECONDS || cloneSampleDuration > MAX_SAMPLE_SECONDS)
    ) {
      setCloneError(
        t('The recording must be between 10 seconds and 5 minutes (detected {seconds} s).', {
          seconds: Math.round(cloneSampleDuration),
        }),
      )
      return
    }
    setCloneSubmitting(true)
    setCloneError('')
    try {
      const created = await api.createVoiceClone({
        name: cloneName.trim(),
        model: cloneModel,
        preview_text: clonePreviewText.trim(),
        authorized: cloneAuthorized,
        file: cloneFile,
      })
      setCloneOpen(false)
      setClones((previous) => [created, ...previous.filter((item) => item.id !== created.id)])
      if (created.model && speechModels.some((item) => item.id === created.model)) {
        setModel(created.model)
      }
      setVoiceId(created.voice_id)
      if (created.preview_warning) {
        toast.error(created.preview_warning)
      } else {
        toast.success(t("Voice cloned. Listen to the preview, then generate speech with it."))
      }
    } catch (err) {
      setCloneError(err instanceof Error ? err.message : t("Failed to clone the voice"))
    } finally {
      setCloneSubmitting(false)
    }
  }

  const regeneratePreview = async (clone: VoiceClone) => {
    setPreviewingCloneId(clone.id)
    try {
      const updated = await api.previewVoiceClone(clone.id, clone.preview_text)
      setClones((previous) => previous.map((item) => (item.id === updated.id ? updated : item)))
      setPreviewNonces((previous) => ({ ...previous, [updated.id]: (previous[updated.id] ?? 0) + 1 }))
      toast.success(t("Preview generated"))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t("Failed to generate a preview"))
    } finally {
      setPreviewingCloneId(null)
    }
  }

  const submitBackgroundTask = async () => {
    setSubmitting(true)
    try {
      const clip = await api.createSpeech({
        model,
        text: text.trim(),
        voice_id: voiceId,
        voice_name: selectedClone?.name ?? selectedVoice?.name ?? '',
        speed,
      })
      setClips((previous) => [clip, ...previous.filter((item) => item.id !== clip.id)])
      setTotal((previous) => previous + 1)
      setText('')
      if (isPending(clip)) {
        toast.success(t('Long text submitted as one background task'))
      } else {
        toast.success(t("Speech generated"))
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t("Failed to generate speech"))
    } finally {
      setSubmitting(false)
    }
  }

  const startStreaming = () => {
    const player = new SpeechStreamPlayer()
    playerRef.current = player
    clipSavedRef.current = false
    setStreamNotice('')
    setLive(true)
    setPhase('streaming')
    setProgress({ generated: 0, played: 0 })
    void player
      .start(
        {
          model,
          text: text.trim(),
          voice_id: voiceId,
          voice_name: selectedClone?.name ?? selectedVoice?.name ?? '',
          speed,
        },
        {
          onProgress: (value) => {
            if (!player.isStopped) setProgress(value)
          },
          onClip: (clip) => {
            clipSavedRef.current = true
            setClips((previous) => [clip, ...previous.filter((item) => item.id !== clip.id)])
            setTotal((previous) => previous + 1)
            setText('')
            setPhase('playing')
            toast.success(t("Speech generated"))
          },
          onPlaybackEnd: () => {
            if (player.isStopped) return
            playerRef.current = null
            setLive(false)
          },
        },
      )
      .catch((err: unknown) => {
        if (player.isStopped) return
        playerRef.current = null
        setLive(false)
        toast.error(err instanceof Error ? err.message : t("Failed to generate speech"))
      })
  }

  const submit = () => {
    if (!model) {
      toast.error(t("Add a MiniMax channel with a speech model first"))
      return
    }
    if (!text.trim()) {
      toast.error(t("Enter the text you want to hear"))
      return
    }
    if (!voiceId) {
      toast.error(t("Choose a voice"))
      return
    }
    if (backgroundTask) {
      void submitBackgroundTask()
      return
    }
    startStreaming()
  }

  const stopPlayback = () => {
    const player = playerRef.current
    if (!player) return
    player.stop()
    playerRef.current = null
    setLive(false)
    setStreamNotice(
      clipSavedRef.current
        ? t('Playback stopped. The completed clip is saved in the history below and can be replayed there.')
        : t('Playback stopped before generation finished, so no clip was saved. Choose “Generate and play” to start again.'),
    )
  }

  return (
    <div className="page">
      <PageHeader
        title={t("Text to speech")}
        subtitle={t('MiniMax speech synthesis from the console. Short text starts playing while it is still being synthesized; long text is submitted as one asynchronous task that survives a page refresh or a gateway restart.')}
        actions={
          <>
            <button className="btn btn-ghost" onClick={refresh} disabled={loading}>
              <Icon name="refresh" size={16} /> {t("Refresh")}
            </button>
            <button className="btn btn-primary" onClick={openCloneModal} disabled={!canClone}>
              <Icon name="upload" size={16} /> {t("Clone a voice")}
            </button>
          </>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> {t("Loading text to speech…")}
        </div>
      ) : (
        <div className="speech-layout">
          <section className="panel speech-create">
            <div className="panel-head">
              <div>
                <h2>{t("Generate speech")}</h2>
                <p>{t("Choose a voice and let MiniMax read your text aloud.")}</p>
              </div>
            </div>
            {speechModels.length === 0 ? (
              <EmptyState
                icon="speech"
                title={t("No speech model")}
                description={t("Add a MiniMax channel with a speech model such as speech-2.8-hd, then come back here to generate speech. Fetch models on the channel adds the MiniMax speech family.")}
                action={
                  <Link className="btn btn-primary" to="/channels">
                    <Icon name="plus" size={16} /> {t("Add a channel")}
                  </Link>
                }
              />
            ) : (
              <form
                className="speech-form"
                onSubmit={(event) => {
                  event.preventDefault()
                  void submit()
                }}
              >
                <FormField label={t("Model")}>
                  <select className="input" value={model} onChange={(event) => setModel(event.target.value)}>
                    {speechModels.map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.id}
                      </option>
                    ))}
                  </select>
                </FormField>
                <FormField label={t("Voice")} hint={voiceHint}>
                  <select
                    className="input"
                    value={voiceId}
                    disabled={voicesLoading || (voices.length === 0 && clones.length === 0)}
                    onChange={(event) => chooseVoice(event.target.value)}
                  >
                    {clones.length === 0 && voiceGroups.length === 0 ? (
                      <option value="">{voicesLoading ? t("Loading voices…") : t("No voices available")}</option>
                    ) : null}
                    {clones.length > 0 ? (
                      <optgroup label={t("My cloned voices")}>
                        {clones.map((clone) => (
                          <option key={clone.voice_id} value={clone.voice_id}>
                            {clone.name || clone.voice_id}
                          </option>
                        ))}
                      </optgroup>
                    ) : null}
                    {voiceGroups.map(([language, list]) => (
                      <optgroup key={language} label={language}>
                        {list.map((voice) => (
                          <option key={voice.voice_id} value={voice.voice_id}>
                            {voice.name || voice.voice_id}
                          </option>
                        ))}
                      </optgroup>
                    ))}
                  </select>
                </FormField>
                {selectedClone ? (
                  <div className="selected-clone">
                    <div className="selected-clone-head">
                      <span className="badge badge-success">
                        <Icon name="check" size={12} /> {t("Cloned voice")}
                      </span>
                      <span className="video-meta">
                        {t('{duration} preview · {format} sample', {
                          duration: formatDuration(selectedClone.preview_duration_ms),
                          format: selectedClone.sample_format.toUpperCase(),
                        })}
                      </span>
                    </div>
                    {selectedClone.preview_ready && selectedClone.preview_url ? (
                      <audio
                        className="speech-audio"
                        controls
                        preload="metadata"
                        src={previewSrc(selectedClone)}
                      >
                        {t("Your browser cannot play this preview.")}
                      </audio>
                    ) : (
                      <div className="speech-note">
                        <Icon name="alert" size={14} />
                        <span>
                          {t("This voice has no preview yet.")}
                          <button
                            className="link-button"
                            onClick={() => void regeneratePreview(selectedClone)}
                            disabled={previewingCloneId === selectedClone.id}
                          >
                            {previewingCloneId === selectedClone.id ? t("Generating…") : t("Generate preview")}
                          </button>
                        </span>
                      </div>
                    )}
                  </div>
                ) : null}
                {voiceWarning ? (
                  <div className="speech-note">
                    <Icon name="alert" size={14} />
                    <span>{voiceWarning}</span>
                  </div>
                ) : null}
                {voiceError ? (
                  <div className="speech-note speech-note-error">
                    <Icon name="alert" size={14} />
                    <span>{voiceError}</span>
                  </div>
                ) : null}
                <FormField
                  label={t("Text")}
                  hint={
                  backgroundTask
                    ? t('{n} / {max} characters · will run in the background', { n: text.length, max: MAX_TEXT })
                    : t('{n} / {max} characters', { n: text.length, max: MAX_TEXT })
                }
                >
                  <textarea
                    className="input textarea"
                    rows={6}
                    maxLength={MAX_TEXT}
                    value={text}
                    onChange={(event) => setText(event.target.value)}
                    placeholder={t('Type or paste the text you want MiniMax to read aloud. Longer text is submitted as one asynchronous task…')}
                  />
                </FormField>
                <div className="field">
                  <span className="param-head">
                    <span className="field-label">{t("Speed")}</span>
                    <span className="param-value">{speed.toFixed(1)}×</span>
                  </span>
                  <input
                    className="param-range"
                    type="range"
                    min={0.5}
                    max={2}
                    step={0.1}
                    value={speed}
                    onChange={(event) => setSpeed(Number.parseFloat(event.target.value))}
                  />
                  <span className="field-hint">{t("0.5× to 2×. The default is 1×.")}</span>
                </div>
                {live ? (
                  <div className="speech-live">
                    <div className="speech-live-copy">
                      <span className="speech-live-bars" aria-hidden="true">
                        <span />
                        <span />
                        <span />
                        <span />
                        <span />
                      </span>
                      <div>
                        <div className="speech-live-title" role="status">
                          {phase === 'streaming' ? t('Generating and playing…') : t('Playing generated audio…')}
                        </div>
                        <div className="speech-live-meta">
                          {t('{played} played · {generated} received', {
                            played: formatClock(progress.played),
                            generated: formatClock(progress.generated),
                          })}
                        </div>
                      </div>
                    </div>
                    <button className="btn btn-sm btn-danger" type="button" onClick={stopPlayback}>
                      <Icon name="stop" size={15} /> {t('Stop')}
                    </button>
                  </div>
                ) : backgroundTask ? (
                  <button
                    className="btn btn-primary btn-block"
                    type="submit"
                    disabled={submitting || !text.trim() || !voiceId}
                  >
                    {submitting ? <Spinner size={16} /> : <Icon name="speech" size={16} />}
                    {submitting ? t("Starting…") : t("Start background task")}
                  </button>
                ) : (
                  <button className="btn btn-primary btn-block" type="submit" disabled={!text.trim() || !voiceId}>
                    <Icon name="speech" size={16} /> {t('Generate and play')}
                  </button>
                )}
                {streamNotice ? (
                  <p className="speech-status" role="status">
                    {streamNotice}
                  </p>
                ) : null}
                <span className="field-hint">
                  {t(
                    'Short text streams: playback starts with the first chunk, and Stop cancels the request. Text over {n} characters is submitted as one asynchronous MiniMax task; the task id is saved on the gateway, so a refresh, a new tab or a gateway restart keeps following the same task and its download.',
                    { n: SYNC_TEXT_LIMIT.toLocaleString() },
                  )}
                </span>
              </form>
            )}
          </section>

          <div className="speech-side">
            <section className="panel speech-clones">
              <div className="panel-head">
                <div>
                  <h2>{t("Cloned voices")}</h2>
                  <p>
                    {clones.length === 0
                      ? t("No cloned voices yet")
                      : clones.length === 1
                      ? t('{count} custom voice created from authorized samples', { count: clones.length })
                      : t('{count} custom voices created from authorized samples', { count: clones.length })}
                  </p>
                </div>
              </div>
              {clones.length === 0 ? (
                <EmptyState
                  icon="speech"
                  title={t("No cloned voices yet")}
                  description={t("Upload a recording you are authorized to use. MiniMax clones it into a custom voice, the preview is kept here, and you can select it to read new text.")}
                  action={
                    <button className="btn btn-primary" onClick={openCloneModal} disabled={!canClone}>
                      <Icon name="upload" size={16} /> {t("Clone a voice")}
                    </button>
                  }
                />
              ) : (
                <div className="voice-clone-list">
                  {clones.map((clone) => (
                    <CloneCard
                      key={clone.id}
                      clone={clone}
                      selected={clone.voice_id === voiceId}
                      previewing={previewingCloneId === clone.id}
                      previewSrc={previewSrc(clone)}
                      onUse={() => chooseVoice(clone.voice_id)}
                      onRegenerate={() => void regeneratePreview(clone)}
                    />
                  ))}
                </div>
              )}
            </section>

            <section className="panel speech-history">
              <div className="panel-head">
                <div>
                  <h2>{t("Generated speech")}</h2>
                  <p>
                    {total === 0
                      ? t('No speech yet')
                      : total === 1
                        ? t('{total} clip in this workspace', { total })
                        : t('{total} clips in this workspace', { total })}
                    {pending ? ' · updating automatically' : ''}
                  </p>
                </div>
                {pending ? <Spinner size={16} /> : null}
              </div>
              {clips.length === 0 ? (
                <EmptyState
                  icon="speech"
                  title={t("No speech yet")}
                  description={t("Choose a system voice, enter some text, and generate your first clip. Long text continues in the background and appears here with live progress.")}
                />
              ) : (
                <div className="speech-list">
                  {clips.map((clip) => (
                    <SpeechCard key={clip.id} clip={clip} />
                  ))}
                </div>
              )}
            </section>
          </div>
        </div>
      )}

      <Modal
        open={cloneOpen}
        onClose={() => {
          if (!cloneSubmitting) setCloneOpen(false)
        }}
        title={t("Clone a voice")}
        subtitle={t("Create a custom MiniMax voice from a recording you are authorized to use, then preview it before speaking new text.")}
        width={640}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setCloneOpen(false)} disabled={cloneSubmitting}>
              {t("Cancel")}
            </button>
            <button className="btn btn-primary" onClick={() => void submitClone()} disabled={cloneSubmitting}>
              {cloneSubmitting ? <Spinner size={15} /> : <Icon name="upload" size={15} />}
              {cloneSubmitting ? t("Cloning voice…") : t("Clone voice")}
            </button>
          </>
        }
      >
        {cloneError ? <ErrorBanner message={cloneError} /> : null}
        <div className="speech-form">
          <FormField label={t("Voice name")} htmlFor="clone-name">
            <input
              id="clone-name"
              className="input"
              value={cloneName}
              maxLength={80}
              onChange={(event) => setCloneName(event.target.value)}
              placeholder={t("My narration voice")}
            />
          </FormField>
          <div className="field">
            <span className="field-label">{t("Sample recording")}</span>
            {recordingSupported ? (
              <div className="clone-source-tabs" role="tablist" aria-label={t("Sample source")}>
                <button
                  type="button"
                  role="tab"
                  aria-selected={cloneSource === 'upload'}
                  className={cloneSource === 'upload' ? 'active' : ''}
                  onClick={() => switchCloneSource('upload')}
                  disabled={cloneSubmitting}
                >
                  <Icon name="upload" size={14} /> {t("Upload a file")}
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={cloneSource === 'record'}
                  className={cloneSource === 'record' ? 'active' : ''}
                  onClick={() => switchCloneSource('record')}
                  disabled={cloneSubmitting}
                >
                  <Icon name="mic" size={14} /> {t("Record now")}
                </button>
              </div>
            ) : null}
            {cloneSource === 'upload' ? (
              <>
                <input
                  id="clone-file"
                  className="input file-input"
                  type="file"
                  aria-label={t("Sample recording file")}
                  accept=".mp3,.m4a,.wav,audio/mpeg,audio/mp4,audio/wav"
                  onChange={(event) => pickCloneFile(event.target.files?.[0] ?? null)}
                />
                <span className="field-hint">
                  {cloneSampleDuration !== null
                    ? t('{name} · {duration} · mp3, m4a or wav', {
                          name: cloneFile?.name ?? t('Recording'),
                          duration: formatDuration(cloneSampleDuration * 1000),
                        })
                    : t("mp3, m4a or wav · 10 seconds to 5 minutes · up to 20 MB.")}
                </span>
              </>
            ) : (
              <div className="recorder">
                {recording ? (
                  <>
                    <div className="recorder-live">
                      <span className="recorder-dot" />
                      <span>{t('Recording… {duration}', { duration: formatDuration(recordingSeconds * 1000) })}</span>
                    </div>
                    <button
                      type="button"
                      className="btn btn-primary"
                      onClick={stopRecording}
                      disabled={recordingSeconds < MIN_SAMPLE_SECONDS}
                    >
                      <Icon name="stop" size={15} /> {t("Stop recording")}
                    </button>
                    <span className="field-hint">
                      {recordingSeconds < MIN_SAMPLE_SECONDS
                        ? t('Keep recording for at least {seconds} more seconds.', {
                          seconds: Math.ceil(MIN_SAMPLE_SECONDS - recordingSeconds),
                        })
                        : t("Stops automatically at 5 minutes. The recording stays on this page until you submit.")}
                    </span>
                  </>
                ) : preparingRecording ? (
                  <div className="recorder-preparing">
                    <Spinner size={15} /> {t("Preparing the recording…")}
                  </div>
                ) : recordingApplied && cloneFile ? (
                  <>
                    <div className="recorder-ready">
                      <Icon name="check" size={14} />{' '}
                      {t('Recording ready · {duration} · {size}', {
                        duration: formatDuration((cloneSampleDuration ?? 0) * 1000),
                        size: formatBytes(cloneFile.size),
                      })}
                    </div>
                    {recordingUrl ? <audio className="speech-audio" controls preload="metadata" src={recordingUrl} /> : null}
                    <div className="recorder-actions">
                      <button type="button" className="btn btn-ghost btn-sm" onClick={() => void startRecording()}>
                        <Icon name="refresh" size={14} /> {t("Re-record")}
                      </button>
                      <button type="button" className="btn btn-ghost btn-sm" onClick={clearRecording}>
                        <Icon name="trash" size={14} /> {t("Remove")}
                      </button>
                    </div>
                    <span className="field-hint">
                      {t("The recording is converted to WAV on this page and sent to MiniMax only when you submit.")}
                    </span>
                  </>
                ) : (
                  <>
                    <button type="button" className="btn btn-primary" onClick={() => void startRecording()}>
                      <Icon name="mic" size={15} /> {t("Start recording")}
                    </button>
                    <span className="field-hint">
                      {t("Allow microphone access, then record 10 seconds to 5 minutes. The recording is converted to WAV locally and uploaded only when you submit.")}
                    </span>
                  </>
                )}
              </div>
            )}
            {recordingError ? (
              <div className="speech-note speech-note-error">
                <Icon name="alert" size={14} />
                <span>{recordingError}</span>
              </div>
            ) : null}
            {!recordingSupported ? (
              <span className="field-hint">
                {t("Recording needs microphone access and a secure (HTTPS or localhost) page; upload a file instead.")}
              </span>
            ) : null}
          </div>
          <FormField label={t("Preview model")}>
            <select
              className="input"
              value={cloneModel}
              onChange={(event) => setCloneModel(event.target.value)}
            >
              {speechModels.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.id}
                </option>
              ))}
            </select>
          </FormField>
          <FormField
            label={t("Preview text")}
            hint={t('{count} / {max} characters. The new voice reads this text once so you can judge the result.', {
              count: clonePreviewText.length,
              max: MAX_PREVIEW_TEXT,
            })}
          >
            <textarea
              className="input textarea"
              rows={3}
              maxLength={MAX_PREVIEW_TEXT}
              value={clonePreviewText}
              onChange={(event) => setClonePreviewText(event.target.value)}
            />
          </FormField>
          <label className="checkbox clone-consent">
            <input
              type="checkbox"
              checked={cloneAuthorized}
              onChange={(event) => setCloneAuthorized(event.target.checked)}
            />
            {t("I confirm I have permission to use this recording and clone this voice.")}
          </label>
          <div className="speech-note">
            <Icon name="shield" size={14} />
            <span>
              {t("Only upload a voice you own or have explicit permission to clone. The sample is sent to MiniMax for cloning; the gateway keeps the generated preview and the voice metadata.")}
            </span>
          </div>
        </div>
      </Modal>
    </div>
  )
}
