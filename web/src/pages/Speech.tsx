import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Modal } from '../components/Modal'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { relativeTime } from '../lib/format'
import { useToast } from '../lib/toast'
import type { ModelInfo, SpeechClip, SpeechVoice, VoiceClone } from '../lib/types'

const MAX_TEXT = 10000
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

function fileExtension(name: string): string {
  const index = name.lastIndexOf('.')
  return index >= 0 ? name.slice(index + 1).toLowerCase() : ''
}

function SpeechCard({ clip }: { clip: SpeechClip }) {
  const ready = clip.audio_ready && clip.content_url
  const format = (clip.format || 'mp3').toUpperCase()
  const size = formatBytes(clip.size_bytes)
  return (
    <article className="speech-card">
      <div className="speech-card-head">
        <div>
          <div className="speech-voice">{clip.voice_name || clip.voice_id}</div>
          <div className="video-meta">
            {clip.model} · {formatDuration(clip.duration_ms)} · {clip.characters} characters
            {size ? ` · ${size}` : ''} · {relativeTime(clip.created_at)}
          </div>
        </div>
        <span className="badge badge-muted">{format}</span>
      </div>
      <p className="speech-text">{clip.text}</p>
      {ready ? (
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
          <span className="video-meta">Audio unavailable</span>
        )}
        <span className="video-meta">by {clip.channel_name}</span>
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
            <Icon name="check" size={12} /> Selected
          </span>
        ) : (
          <span className="badge badge-muted">Cloned</span>
        )}
      </div>
      <div className="video-meta voice-clone-meta">
        {clone.model} · {clone.sample_format.toUpperCase()}
        {sampleSize ? ` · ${sampleSize}` : ''} · {relativeTime(clone.created_at)}
      </div>
      <p className="speech-text">{clone.preview_text}</p>
      {ready ? (
        <audio className="speech-audio" controls preload="metadata" src={previewSrc}>
          Your browser cannot play this preview.
        </audio>
      ) : (
        <div className="speech-note">
          <Icon name="alert" size={14} />
          <span>
            The preview is not ready yet.
            <button className="link-button" onClick={onRegenerate} disabled={previewing}>
              {previewing ? 'Generating…' : 'Generate preview'}
            </button>
          </span>
        </div>
      )}
      <div className="video-actions">
        <button className="btn btn-sm btn-ghost" onClick={onUse} disabled={selected}>
          {selected ? <Icon name="check" size={15} /> : <Icon name="speech" size={15} />}
          {selected ? 'In use' : 'Use voice'}
        </button>
        {ready ? (
          <button className="btn btn-sm btn-ghost" onClick={onRegenerate} disabled={previewing}>
            <Icon name="refresh" size={14} /> {previewing ? 'Generating…' : 'New preview'}
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
  const [clones, setClones] = useState<VoiceClone[]>([])
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

  const speechModels = useMemo(() => models.filter(isSpeechModel), [models])
  const selectedVoice = useMemo(() => voices.find((voice) => voice.voice_id === voiceId), [voices, voiceId])
  const selectedClone = useMemo(
    () => clones.find((clone) => clone.voice_id === voiceId),
    [clones, voiceId],
  )
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

  const previewSrc = useCallback(
    (clone: VoiceClone) =>
      clone.preview_url ? `${clone.preview_url}?v=${previewNonces[clone.id] ?? 0}` : '',
    [previewNonces],
  )

  const voiceHint = voicesLoading
    ? 'Loading system voices from MiniMax…'
    : selectedClone
      ? `Cloned from ${selectedClone.source_filename} on ${selectedClone.channel_name}. Listen to the preview, then generate speech with it.`
      : selectedVoice
        ? `${selectedVoice.description ? `${selectedVoice.description} ` : ''}ID: ${selectedVoice.voice_id}`
        : clones.length > 0
          ? 'Choose a system voice or one of your cloned voices.'
          : 'No voices are available.'

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
        setVoiceError(err instanceof Error ? err.message : 'Failed to load system voices')
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

  const refreshClips = useCallback(async () => {
    try {
      const response = await api.speech()
      setClips(response.items)
      setTotal(response.total)
    } catch {
      // Refreshing is best effort; the visible history stays as-is.
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

  const refresh = () => {
    void refreshClips()
    void refreshClones()
    setVoiceNonce((previous) => previous + 1)
  }

  const openCloneModal = () => {
    setCloneName('')
    setCloneFile(null)
    setCloneSampleDuration(null)
    setClonePreviewText(DEFAULT_CLONE_PREVIEW)
    setCloneModel(model || speechModels[0]?.id || '')
    setCloneAuthorized(false)
    setCloneError('')
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
      setCloneError('Choose an mp3, m4a or wav recording.')
      return
    }
    if (file.size > MAX_SAMPLE_BYTES) {
      setCloneError('The recording must be 20 MB or smaller.')
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

  const submitClone = async () => {
    if (!cloneName.trim()) {
      setCloneError('Give the cloned voice a name.')
      return
    }
    if (!cloneFile) {
      setCloneError('Choose the sample recording to clone.')
      return
    }
    if (!SAMPLE_EXTENSIONS.includes(fileExtension(cloneFile.name))) {
      setCloneError('Choose an mp3, m4a or wav recording.')
      return
    }
    if (cloneFile.size > MAX_SAMPLE_BYTES) {
      setCloneError('The recording must be 20 MB or smaller.')
      return
    }
    if (!clonePreviewText.trim()) {
      setCloneError('Enter the text to preview with the cloned voice.')
      return
    }
    if (!cloneModel) {
      setCloneError('Choose a speech model for the preview.')
      return
    }
    if (!cloneAuthorized) {
      setCloneError('Confirm that you have permission to use this recording.')
      return
    }
    if (
      cloneSampleDuration !== null &&
      (cloneSampleDuration < MIN_SAMPLE_SECONDS || cloneSampleDuration > MAX_SAMPLE_SECONDS)
    ) {
      setCloneError(
        `The recording must be between 10 seconds and 5 minutes (detected ${Math.round(cloneSampleDuration)} s).`,
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
        toast.success('Voice cloned. Listen to the preview, then generate speech with it.')
      }
    } catch (err) {
      setCloneError(err instanceof Error ? err.message : 'Failed to clone the voice')
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
      toast.success('Preview generated')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to generate a preview')
    } finally {
      setPreviewingCloneId(null)
    }
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
      toast.error('Choose a voice')
      return
    }
    setSubmitting(true)
    try {
      const clip = await api.createSpeech({
        model,
        text: text.trim(),
        voice_id: voiceId,
        voice_name: selectedClone?.name ?? selectedVoice?.name ?? '',
        speed,
      })
      setClips((previous) => [clip, ...previous])
      setTotal((previous) => previous + 1)
      toast.success('Speech generated')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to generate speech')
    } finally {
      setSubmitting(false)
    }
  }

  const canClone = speechModels.length > 0 && !loading

  return (
    <div className="page">
      <PageHeader
        title="Text to speech"
        subtitle="MiniMax speech synthesis from the console. Pick a system or cloned voice, generate spoken audio, then preview it and download the file."
        actions={
          <>
            <button className="btn btn-ghost" onClick={refresh} disabled={loading}>
              <Icon name="refresh" size={16} /> Refresh
            </button>
            <button className="btn btn-primary" onClick={openCloneModal} disabled={!canClone}>
              <Icon name="upload" size={16} /> Clone a voice
            </button>
          </>
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
                <FormField label="Voice" hint={voiceHint}>
                  <select
                    className="input"
                    value={voiceId}
                    disabled={voicesLoading || (voices.length === 0 && clones.length === 0)}
                    onChange={(event) => chooseVoice(event.target.value)}
                  >
                    {clones.length === 0 && voiceGroups.length === 0 ? (
                      <option value="">{voicesLoading ? 'Loading voices…' : 'No voices available'}</option>
                    ) : null}
                    {clones.length > 0 ? (
                      <optgroup label="My cloned voices">
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
                        <Icon name="check" size={12} /> Cloned voice
                      </span>
                      <span className="video-meta">
                        {formatDuration(selectedClone.preview_duration_ms)} preview ·{' '}
                        {selectedClone.sample_format.toUpperCase()} sample
                      </span>
                    </div>
                    {selectedClone.preview_ready && selectedClone.preview_url ? (
                      <audio
                        className="speech-audio"
                        controls
                        preload="metadata"
                        src={previewSrc(selectedClone)}
                      >
                        Your browser cannot play this preview.
                      </audio>
                    ) : (
                      <div className="speech-note">
                        <Icon name="alert" size={14} />
                        <span>
                          This voice has no preview yet.
                          <button
                            className="link-button"
                            onClick={() => void regeneratePreview(selectedClone)}
                            disabled={previewingCloneId === selectedClone.id}
                          >
                            {previewingCloneId === selectedClone.id ? 'Generating…' : 'Generate preview'}
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
                <FormField label="Text" hint={`${text.length} / ${MAX_TEXT} characters`}>
                  <textarea
                    className="input textarea"
                    rows={6}
                    maxLength={MAX_TEXT}
                    value={text}
                    onChange={(event) => setText(event.target.value)}
                    placeholder="Type or paste the text you want MiniMax to read aloud…"
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
                  {submitting ? 'Generating speech…' : 'Generate speech'}
                </button>
                <span className="field-hint">
                  Synthesis runs while this page waits. The audio is stored on the gateway, so it can be replayed and
                  downloaded from the history below.
                </span>
              </form>
            )}
          </section>

          <div className="speech-side">
            <section className="panel speech-clones">
              <div className="panel-head">
                <div>
                  <h2>Cloned voices</h2>
                  <p>
                    {clones.length === 0
                      ? 'No cloned voices yet'
                      : `${clones.length} custom voice${clones.length === 1 ? '' : 's'} created from authorized samples`}
                  </p>
                </div>
              </div>
              {clones.length === 0 ? (
                <EmptyState
                  icon="speech"
                  title="No cloned voices yet"
                  description="Upload a recording you are authorized to use. MiniMax clones it into a custom voice, the preview is kept here, and you can select it to read new text."
                  action={
                    <button className="btn btn-primary" onClick={openCloneModal} disabled={!canClone}>
                      <Icon name="upload" size={16} /> Clone a voice
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
                  <h2>Generated speech</h2>
                  <p>{total === 0 ? 'No speech yet' : `${total} clip${total === 1 ? '' : 's'} in this workspace`}</p>
                </div>
              </div>
              {clips.length === 0 ? (
                <EmptyState
                  icon="speech"
                  title="No speech yet"
                  description="Choose a voice, enter some text, and generate your first clip. It will appear here with a player and a download action."
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
        title="Clone a voice"
        subtitle="Create a custom MiniMax voice from a recording you are authorized to use, then preview it before speaking new text."
        width={640}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setCloneOpen(false)} disabled={cloneSubmitting}>
              Cancel
            </button>
            <button className="btn btn-primary" onClick={() => void submitClone()} disabled={cloneSubmitting}>
              {cloneSubmitting ? <Spinner size={15} /> : <Icon name="upload" size={15} />}
              {cloneSubmitting ? 'Cloning voice…' : 'Clone voice'}
            </button>
          </>
        }
      >
        {cloneError ? <ErrorBanner message={cloneError} /> : null}
        <div className="speech-form">
          <FormField label="Voice name" htmlFor="clone-name">
            <input
              id="clone-name"
              className="input"
              value={cloneName}
              maxLength={80}
              onChange={(event) => setCloneName(event.target.value)}
              placeholder="My narration voice"
            />
          </FormField>
          <FormField
            label="Sample recording"
            hint={
              cloneSampleDuration !== null
                ? `${cloneFile?.name ?? 'Recording'} · ${formatDuration(cloneSampleDuration * 1000)} · mp3, m4a or wav`
                : 'mp3, m4a or wav · 10 seconds to 5 minutes · up to 20 MB.'
            }
          >
            <input
              id="clone-file"
              className="input file-input"
              type="file"
              accept=".mp3,.m4a,.wav,audio/mpeg,audio/mp4,audio/wav"
              onChange={(event) => pickCloneFile(event.target.files?.[0] ?? null)}
            />
          </FormField>
          <FormField label="Preview model">
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
            label="Preview text"
            hint={`${clonePreviewText.length} / ${MAX_PREVIEW_TEXT} characters. The new voice reads this text once so you can judge the result.`}
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
            I confirm I have permission to use this recording and clone this voice.
          </label>
          <div className="speech-note">
            <Icon name="shield" size={14} />
            <span>
              Only upload a voice you own or have explicit permission to clone. The sample is sent to MiniMax for
              cloning; the gateway keeps the generated preview and the voice metadata.
            </span>
          </div>
        </div>
      </Modal>
    </div>
  )
}
