import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { relativeTime } from '../lib/format'
import { SPEECH_DEFAULT_MODEL, SPEECH_MAX_TEXT, formatSpeechDuration, isSpeechModel } from '../lib/speech'
import { useToast } from '../lib/toast'
import type { ModelInfo, SpeechClip, VoiceDesign } from '../lib/types'

const MAX_PROMPT = 2000
const MAX_PREVIEW_TEXT = 500
const DEFAULT_PREVIEW_TEXT =
  'Hello! This is a preview of the voice designed from your description. You can use this voice to read any text aloud.'
const EXAMPLE_PROMPT =
  'A warm, calm female narrator in her thirties with a gentle, unhurried delivery and a hint of curiosity.'

function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function DesignCard({
  design,
  selected,
  onSelect,
}: {
  design: VoiceDesign
  selected: boolean
  onSelect: () => void
}) {
  const format = (design.preview_format || 'mp3').toUpperCase()
  const size = formatBytes(design.preview_size_bytes)
  return (
    <article className={`speech-card voice-design-card${selected ? ' voice-design-card-selected' : ''}`}>
      <div className="speech-card-head">
        <div>
          <div className="speech-voice">{design.name || design.voice_id}</div>
          <div className="video-meta">
            {design.model} · {relativeTime(design.created_at)}
            {size ? ` · ${size}` : ''}
          </div>
        </div>
        <span className="badge badge-muted">{format}</span>
      </div>
      <p className="speech-text">{design.prompt}</p>
      {design.preview_ready && design.preview_url ? (
        <audio className="speech-audio" controls preload="metadata" src={design.preview_url}>
          Your browser cannot play this audio.
        </audio>
      ) : (
        <div className="speech-audio-missing">
          <Icon name="alert" size={14} /> The preview audio is not available.
        </div>
      )}
      <div className="video-actions">
        <button className="btn btn-sm btn-ghost" onClick={onSelect} disabled={selected}>
          <Icon name={selected ? 'check' : 'speech'} size={15} />
          {selected ? 'Selected' : 'Use this voice'}
        </button>
        {design.preview_ready && design.preview_url ? (
          <a className="btn btn-sm btn-ghost" href={`${design.preview_url}?download=1`}>
            <Icon name="download" size={15} /> Download
          </a>
        ) : null}
      </div>
      <div className="speech-voice-id mono">{design.voice_id}</div>
    </article>
  )
}

export default function VoiceDesignPage() {
  const toast = useToast()
  const [models, setModels] = useState<ModelInfo[]>([])
  const [designs, setDesigns] = useState<VoiceDesign[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [previewText, setPreviewText] = useState(DEFAULT_PREVIEW_TEXT)
  const [designing, setDesigning] = useState(false)

  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [readText, setReadText] = useState('')
  const [speed, setSpeed] = useState(1)
  const [speaking, setSpeaking] = useState(false)
  const [clip, setClip] = useState<SpeechClip | null>(null)

  const speechModels = useMemo(() => models.filter(isSpeechModel), [models])
  const selected = useMemo(
    () => designs.find((design) => design.id === selectedId) ?? null,
    [designs, selectedId],
  )

  useEffect(() => {
    let active = true
    void (async () => {
      try {
        const [modelResponse, designResponse] = await Promise.all([api.models(), api.voiceDesigns()])
        if (!active) return
        setModels(modelResponse.data)
        setDesigns(designResponse.items)
        setSelectedId((previous) => previous ?? designResponse.items[0]?.id ?? null)
      } catch (err) {
        if (active) setError(err instanceof Error ? err.message : 'Failed to load voice design')
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
      if (speechModels.some((item) => item.id === SPEECH_DEFAULT_MODEL)) return SPEECH_DEFAULT_MODEL
      return speechModels[0]?.id ?? ''
    })
  }, [speechModels])

  useEffect(() => {
    setClip(null)
  }, [selectedId])

  const refresh = useCallback(async () => {
    try {
      const [modelResponse, designResponse] = await Promise.all([api.models(), api.voiceDesigns()])
      setModels(modelResponse.data)
      setDesigns(designResponse.items)
      setSelectedId((previous) => {
        if (previous && designResponse.items.some((design) => design.id === previous)) return previous
        return designResponse.items[0]?.id ?? null
      })
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to refresh voice design')
    }
  }, [toast])

  const design = async () => {
    if (!model) {
      toast.error('Add a MiniMax channel with a speech model first')
      return
    }
    if (!prompt.trim()) {
      toast.error('Describe the voice you want to design')
      return
    }
    if (!previewText.trim()) {
      toast.error('Enter the preview text for the voice sample')
      return
    }
    setDesigning(true)
    try {
      const created = await api.createVoiceDesign({
        model,
        prompt: prompt.trim(),
        preview_text: previewText.trim(),
      })
      setDesigns((previous) => [created, ...previous])
      setSelectedId(created.id)
      toast.success('Voice designed')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to design the voice')
    } finally {
      setDesigning(false)
    }
  }

  const speak = async () => {
    if (!selected) return
    if (!readText.trim()) {
      toast.error('Enter the text this voice should read')
      return
    }
    setSpeaking(true)
    try {
      const created = await api.speakWithVoiceDesign(selected.id, {
        text: readText.trim(),
        speed,
      })
      setClip(created)
      toast.success('Speech generated')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to generate speech')
    } finally {
      setSpeaking(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title="Voice design"
        subtitle="Describe a voice, preview the sample MiniMax generates, then use that voice to read any text."
        actions={
          <button className="btn btn-ghost" onClick={() => void refresh()} disabled={loading}>
            <Icon name="refresh" size={16} /> Refresh
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading voice design…
        </div>
      ) : speechModels.length === 0 ? (
        <EmptyState
          icon="speech"
          title="No speech model"
          description="Add a MiniMax channel with a speech model such as speech-2.8-hd, then come back here to design a voice from a description."
          action={
            <Link className="btn btn-primary" to="/channels">
              <Icon name="plus" size={16} /> Add a channel
            </Link>
          }
        />
      ) : (
        <div className="speech-layout">
          <section className="panel speech-create">
            <div className="panel-head">
              <div>
                <h2>Design a voice</h2>
                <p>Describe the voice you want. MiniMax creates a custom voice and reads the preview text as a sample.</p>
              </div>
            </div>
            <form
              className="speech-form"
              onSubmit={(event) => {
                event.preventDefault()
                void design()
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
              <FormField label="Voice description" hint={`${prompt.length} / ${MAX_PROMPT} characters`}>
                <textarea
                  className="input textarea"
                  rows={5}
                  maxLength={MAX_PROMPT}
                  value={prompt}
                  onChange={(event) => setPrompt(event.target.value)}
                  placeholder={EXAMPLE_PROMPT}
                />
              </FormField>
              <FormField label="Preview text" hint={`${previewText.length} / ${MAX_PREVIEW_TEXT} characters`}>
                <textarea
                  className="input textarea"
                  rows={4}
                  maxLength={MAX_PREVIEW_TEXT}
                  value={previewText}
                  onChange={(event) => setPreviewText(event.target.value)}
                  placeholder="The sample sentence the designed voice reads in its preview."
                />
              </FormField>
              <div className="speech-note">
                <Icon name="alert" size={14} />
                <span>
                  Designing generates a paid MiniMax preview sample ($30 per 1M characters). The preview text is capped
                  at {MAX_PREVIEW_TEXT} characters.
                </span>
              </div>
              <button
                className="btn btn-primary btn-block"
                type="submit"
                disabled={designing || !prompt.trim() || !previewText.trim()}
              >
                {designing ? <Spinner size={16} /> : <Icon name="spark" size={16} />}
                {designing ? 'Designing voice…' : 'Design voice'}
              </button>
              <span className="field-hint">
                The designed voice and its preview are stored on the gateway. Pick it in the list to replay the sample.
              </span>
            </form>
          </section>

          <section className="panel speech-history voice-design-studio">
            <div className="panel-head">
              <div>
                <h2>Use a designed voice</h2>
                <p>
                  {selected
                    ? 'Preview the sample, then read another piece of text with the selected voice.'
                    : 'Design a voice, or select one from the list, to preview it and read new text.'}
                </p>
              </div>
            </div>

            {designs.length === 0 ? (
              <EmptyState
                icon="spark"
                title="No designed voices"
                description="Describe a voice on the left, generate its preview, and it will appear here so you can read new text with it."
              />
            ) : (
              <>
                {selected ? (
                  <div className="voice-design-selected">
                    <div className="voice-design-selected-head">
                      <div>
                        <div className="speech-voice">{selected.name || selected.voice_id}</div>
                        <div className="video-meta">
                          {selected.model} · {selected.channel_name} · {relativeTime(selected.created_at)}
                        </div>
                      </div>
                      <span className="badge badge-success">Selected</span>
                    </div>
                    <p className="voice-design-prompt">{selected.prompt}</p>
                    <div className="speech-voice-id mono">{selected.voice_id}</div>
                    {selected.preview_ready && selected.preview_url ? (
                      <div className="voice-design-preview">
                        <span className="field-label">Preview</span>
                        <audio className="speech-audio" controls preload="metadata" src={selected.preview_url}>
                          Your browser cannot play this audio.
                        </audio>
                      </div>
                    ) : null}

                    <form
                      className="speech-form voice-design-read"
                      onSubmit={(event) => {
                        event.preventDefault()
                        void speak()
                      }}
                    >
                      <FormField
                        label="Read another text"
                        hint={`${readText.length} / ${SPEECH_MAX_TEXT} characters`}
                      >
                        <textarea
                          className="input textarea"
                          rows={5}
                          maxLength={SPEECH_MAX_TEXT}
                          value={readText}
                          onChange={(event) => setReadText(event.target.value)}
                          placeholder="Type the text this designed voice should read aloud…"
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
                      </div>
                      <button
                        className="btn btn-primary btn-block"
                        type="submit"
                        disabled={speaking || !readText.trim()}
                      >
                        {speaking ? <Spinner size={16} /> : <Icon name="speech" size={16} />}
                        {speaking ? 'Generating speech…' : 'Read with this voice'}
                      </button>
                    </form>

                    {clip ? (
                      <div className="voice-design-clip">
                        <div className="voice-design-clip-head">
                          <span className="field-label">Generated speech</span>
                          <span className="video-meta">
                            {clip.model} · {formatSpeechDuration(clip.duration_ms)} · {clip.characters} characters
                          </span>
                        </div>
                        {clip.audio_ready && clip.content_url ? (
                          <>
                            <audio className="speech-audio" controls preload="metadata" src={clip.content_url}>
                              Your browser cannot play this audio.
                            </audio>
                            <div className="video-actions">
                              <a className="btn btn-sm btn-ghost" href={`${clip.content_url}?download=1`}>
                                <Icon name="download" size={15} /> Download {(clip.format || 'mp3').toUpperCase()}
                              </a>
                              <Link className="btn btn-sm btn-ghost" to="/speech">
                                Open speech history
                              </Link>
                            </div>
                          </>
                        ) : (
                          <div className="speech-audio-missing">
                            <Icon name="alert" size={14} /> The generated audio is not available.
                          </div>
                        )}
                      </div>
                    ) : null}
                  </div>
                ) : null}

                <div className="voice-design-list-head">
                  <div>
                    <h2>Designed voices</h2>
                    <p>{designs.length} in this workspace</p>
                  </div>
                </div>
                <div className="speech-list">
                  {designs.map((item) => (
                    <DesignCard
                      key={item.id}
                      design={item}
                      selected={item.id === selectedId}
                      onSelect={() => setSelectedId(item.id)}
                    />
                  ))}
                </div>
              </>
            )}
          </section>
        </div>
      )}
    </div>
  )
}
