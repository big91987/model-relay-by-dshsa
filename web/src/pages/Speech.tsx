import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { relativeTime } from '../lib/format'
import { useToast } from '../lib/toast'
import type { ModelInfo, SpeechClip, SpeechClipStatus, SpeechVoice } from '../lib/types'

const MAX_TEXT = 50000
// Text longer than this is accepted as a background job instead of holding the
// page open. Keep in sync with speechSyncLimit in internal/gateway/tts.go.
const SYNC_TEXT_LIMIT = 2000
const DEFAULT_SPEECH_MODEL = 'speech-2.8-hd'
const DEFAULT_VOICE_ID = 'English_expressive_narrator'

function isSpeechModel(model: ModelInfo): boolean {
  if (!model.types.includes('minimax')) return false
  return /^speech-/i.test(model.id) || /^minimax-speech/i.test(model.id)
}

function isPending(clip: SpeechClip): boolean {
  return clip.status === 'queued' || clip.status === 'running'
}

function formatDuration(durationMS: number): string {
  if (!durationMS || durationMS <= 0) return 'Unknown length'
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

function statusLabel(status: SpeechClipStatus): string {
  switch (status) {
    case 'queued':
      return 'Queued'
    case 'running':
      return 'Synthesizing'
    case 'succeeded':
      return 'Ready'
    case 'failed':
      return 'Failed'
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
  return (
    <article className="speech-card">
      <div className="speech-card-head">
        <div>
          <div className="speech-voice">{clip.voice_name || clip.voice_id}</div>
          <div className="video-meta">
            {clip.model} ·{' '}
            {clip.status === 'succeeded' ? `${formatDuration(clip.duration_ms)} · ` : ''}
            {clip.characters.toLocaleString()} characters{size ? ` · ${size}` : ''} · {relativeTime(clip.created_at)}
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
                ? 'Waiting to start…'
                : `Synthesizing part ${Math.min(clip.done_chunks + 1, clip.total_chunks)} of ${clip.total_chunks}…`}
            </span>
            <span className="speech-task-percent">{percent}%</span>
          </div>
          <div
            className="speech-progress"
            role="progressbar"
            aria-valuenow={percent}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-label="Speech synthesis progress"
          >
            <span className="speech-progress-bar" style={{ width: `${percent}%` }} />
          </div>
          <span className="speech-task-note">
            This task keeps running in the background; refresh or leave the page and come back to download it.
          </span>
        </div>
      ) : clip.status === 'failed' ? (
        <div className="speech-audio-missing">
          <Icon name="alert" size={14} /> {clip.error || 'Speech synthesis failed.'}
        </div>
      ) : ready ? (
        <audio className="speech-audio" controls preload="metadata" src={clip.content_url}>
          Your browser cannot play this audio.
        </audio>
      ) : (
        <div className="speech-audio-missing">
          <Icon name="alert" size={14} /> The stored audio file is not available.
        </div>
      )}
      <div className="video-actions">
        {ready ? (
          <a className="btn btn-sm btn-ghost" href={`${clip.content_url}?download=1`}>
            <Icon name="download" size={15} /> Download {format}
          </a>
        ) : (
          <span className="video-meta">
            {clip.status === 'failed' ? 'No audio to download' : `Audio will be ready after ${clip.total_chunks} parts`}
          </span>
        )}
        <span className="video-meta">by {clip.channel_name}</span>
      </div>
      <div className="speech-voice-id mono">{clip.voice_id}</div>
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

  const [model, setModel] = useState('')
  const [text, setText] = useState('')
  const [speed, setSpeed] = useState(1)
  const [voices, setVoices] = useState<SpeechVoice[]>([])
  const [voiceId, setVoiceId] = useState('')
  const [voicesLoading, setVoicesLoading] = useState(false)
  const [voiceWarning, setVoiceWarning] = useState('')
  const [voiceError, setVoiceError] = useState('')
  const [voiceNonce, setVoiceNonce] = useState(0)

  const speechModels = useMemo(() => models.filter(isSpeechModel), [models])
  const selectedVoice = useMemo(() => voices.find((voice) => voice.voice_id === voiceId), [voices, voiceId])
  const pending = clips.some(isPending)
  const backgroundTask = text.trim().length > SYNC_TEXT_LIMIT
  const voiceGroups = useMemo(() => {
    const groups = new Map<string, SpeechVoice[]>()
    for (const voice of voices) {
      const language = voice.language || 'Other'
      const list = groups.get(language) ?? []
      list.push(voice)
      groups.set(language, list)
    }
    return Array.from(groups.entries()).sort(([a], [b]) => a.localeCompare(b))
  }, [voices])

  const voiceHint = voicesLoading
    ? 'Loading system voices from MiniMax…'
    : selectedVoice
      ? `${selectedVoice.description ? `${selectedVoice.description} ` : ''}ID: ${selectedVoice.voice_id}`
      : 'No system voices are available.'

  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const [modelResponse, speechResponse] = await Promise.all([api.models(), api.speech()])
        if (!active) return
        setModels(modelResponse.data)
        setClips(speechResponse.items)
        setTotal(speechResponse.total)
      } catch (err) {
        if (active) setError(err instanceof Error ? err.message : 'Failed to load text to speech')
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
          if (catalog.voices.some((voice) => voice.voice_id === previous)) return previous
          const preferred = catalog.voices.find((voice) => voice.voice_id === DEFAULT_VOICE_ID)
          return preferred?.voice_id ?? catalog.voices[0]?.voice_id ?? ''
        })
      })
      .catch((err: unknown) => {
        if (!active) return
        setVoices([])
        setVoiceWarning('')
        setVoiceError(err instanceof Error ? err.message : 'Failed to load system voices')
      })
      .finally(() => {
        if (active) setVoicesLoading(false)
      })
    return () => {
      active = false
    }
  }, [model, voiceNonce])

  const refreshClips = useCallback(async () => {
    try {
      const response = await api.speech()
      setClips(response.items)
      setTotal(response.total)
    } catch {
      // Polling and manual refreshes are best effort; the visible history stays as-is.
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
    setVoiceNonce((previous) => previous + 1)
  }

  const submit = async () => {
    if (!model) {
      toast.error('Add a MiniMax channel with a speech model first')
      return
    }
    if (!text.trim()) {
      toast.error('Enter the text you want to hear')
      return
    }
    if (!voiceId) {
      toast.error('Choose a system voice')
      return
    }
    setSubmitting(true)
    try {
      const clip = await api.createSpeech({
        model,
        text: text.trim(),
        voice_id: voiceId,
        voice_name: selectedVoice?.name ?? '',
        speed,
      })
      setClips((previous) => [clip, ...previous.filter((item) => item.id !== clip.id)])
      setTotal((previous) => previous + 1)
      setText('')
      if (isPending(clip)) {
        toast.success('Long text started as a background task')
      } else {
        toast.success('Speech generated')
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to generate speech')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title="Text to speech"
        subtitle="MiniMax speech synthesis from the console. Short text is generated immediately; long text runs as a background task that survives a page refresh."
        actions={
          <button className="btn btn-ghost" onClick={refresh} disabled={loading}>
            <Icon name="refresh" size={16} /> Refresh
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading text to speech…
        </div>
      ) : (
        <div className="speech-layout">
          <section className="panel speech-create">
            <div className="panel-head">
              <div>
                <h2>Generate speech</h2>
                <p>Choose a voice and let MiniMax read your text aloud.</p>
              </div>
            </div>
            {speechModels.length === 0 ? (
              <EmptyState
                icon="speech"
                title="No speech model"
                description="Add a MiniMax channel with a speech model such as speech-2.8-hd, then come back here to generate speech. Fetch models on the channel adds the MiniMax speech family."
                action={
                  <Link className="btn btn-primary" to="/channels">
                    <Icon name="plus" size={16} /> Add a channel
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
                <FormField label="Model">
                  <select className="input" value={model} onChange={(event) => setModel(event.target.value)}>
                    {speechModels.map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.id}
                      </option>
                    ))}
                  </select>
                </FormField>
                <FormField label="System voice" hint={voiceHint}>
                  <select
                    className="input"
                    value={voiceId}
                    disabled={voicesLoading || voices.length === 0}
                    onChange={(event) => setVoiceId(event.target.value)}
                  >
                    {voiceGroups.length === 0 ? (
                      <option value="">{voicesLoading ? 'Loading voices…' : 'No voices available'}</option>
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
                  label="Text"
                  hint={`${text.length} / ${MAX_TEXT} characters${backgroundTask ? ' · will run in the background' : ''}`}
                >
                  <textarea
                    className="input textarea"
                    rows={6}
                    maxLength={MAX_TEXT}
                    value={text}
                    onChange={(event) => setText(event.target.value)}
                    placeholder="Type or paste the text you want MiniMax to read aloud. Longer text is split into parts and synthesized in the background…"
                  />
                </FormField>
                <div className="field">
                  <span className="param-head">
                    <span className="field-label">Speed</span>
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
                  <span className="field-hint">0.5× to 2×. The default is 1×.</span>
                </div>
                <button
                  className="btn btn-primary btn-block"
                  type="submit"
                  disabled={submitting || !text.trim() || !voiceId}
                >
                  {submitting ? <Spinner size={16} /> : <Icon name="speech" size={16} />}
                  {submitting
                    ? 'Starting…'
                    : backgroundTask
                      ? 'Start background task'
                      : 'Generate speech'}
                </button>
                <span className="field-hint">
                  Text over {SYNC_TEXT_LIMIT.toLocaleString()} characters is split into parts and synthesized in the
                  background. Progress is saved on the gateway, so the task and its audio are still here after a
                  refresh.
                </span>
              </form>
            )}
          </section>

          <section className="panel speech-history">
            <div className="panel-head">
              <div>
                <h2>Generated speech</h2>
                <p>
                  {total === 0 ? 'No speech yet' : `${total} clip${total === 1 ? '' : 's'} in this workspace`}
                  {pending ? ' · updating automatically' : ''}
                </p>
              </div>
              {pending ? <Spinner size={16} /> : null}
            </div>
            {clips.length === 0 ? (
              <EmptyState
                icon="speech"
                title="No speech yet"
                description="Choose a system voice, enter some text, and generate your first clip. Long text continues in the background and appears here with live progress."
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
      )}
    </div>
  )
}
