import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { relativeTime } from '../lib/format'
import { useToast } from '../lib/toast'
import type { ModelInfo, MusicTask, MusicTaskStatus } from '../lib/types'
import { t } from '../lib/i18n'

const MAX_PROMPT = 2000
const MAX_LYRICS = 3500

function isMusicModel(model: ModelInfo): boolean {
  return model.types.includes('minimax-music')
}

function statusLabel(status: MusicTaskStatus): string {
  switch (status) {
    case 'queued':
      return t("Queued")
    case 'generating':
      return t('Composing')
    case 'succeeded':
      return t('Ready')
    case 'failed':
      return t('Failed')
  }
}

function MusicStatus({ status }: { status: MusicTaskStatus }) {
  const tone = status === 'succeeded' ? 'success' : status === 'failed' ? 'danger' : 'muted'
  return (
    <span className={`badge badge-${tone}`}>
      <span className={`badge-dot badge-dot-${tone}`} />
      {statusLabel(status)}
    </span>
  )
}

function formatSongDuration(milliseconds: number): string {
  if (!milliseconds || milliseconds <= 0) return '—'
  const totalSeconds = Math.round(milliseconds / 1000)
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${minutes}:${String(seconds).padStart(2, '0')}`
}

function MusicCard({ task }: { task: MusicTask }) {
  const ready = task.audio_ready && task.content_url
  const format = task.format === 'wav' ? 'WAV' : 'MP3'
  return (
    <article className="music-card">
      <div className="music-card-head">
        <div>
          <div className="music-model">{task.model}</div>
          <div className="music-meta">
            {task.instrumental ? t('Instrumental') : t('Vocals')} · {format} ·{' '}
            {formatSongDuration(task.duration_ms)} · {relativeTime(task.created_at)}
          </div>
        </div>
        <MusicStatus status={task.status} />
      </div>
      {ready ? (
        <audio className="music-player" controls preload="metadata" src={task.content_url}>
          {t("Your browser cannot play this audio.")}
        </audio>
      ) : (
        <div className={`music-placeholder state-${task.status}`}>
          {task.status === 'failed' ? (
            <Icon name="alert" size={20} />
          ) : task.status === 'succeeded' ? (
            task.error ? <Icon name="alert" size={20} /> : <Icon name="music" size={20} />
          ) : (
            <Spinner size={20} />
          )}
          <span>
            {task.status === 'failed'
              ? t('Generation failed')
              : task.status === 'succeeded'
                ? task.error
                  ? t('Generated, but not stored locally')
                  : t('Song is being stored')
                : task.status === 'queued'
                  ? t('Waiting to compose…')
                  : t('Composing the song…')}
          </span>
        </div>
      )}
      <p className="music-prompt">{task.prompt}</p>
      {task.instrumental ? (
        <span className="music-instrumental">
          <Icon name="music" size={13} /> {t('Instrumental — no lyrics')}
        </span>
      ) : (
        <details className="music-lyrics">
          <summary>{t('Lyrics')}</summary>
          <pre>{task.lyrics}</pre>
        </details>
      )}
      {task.error ? (
        <div className="music-error">
          <Icon name="alert" size={14} />
          <span>{task.error}</span>
        </div>
      ) : null}
      {ready ? (
        <div className="music-actions">
          <a className="btn btn-sm btn-ghost" href={`${task.content_url}?download=1`}>
            <Icon name="download" size={15} /> {t('Download {format}', { format })}
          </a>
          <span className="music-meta">{t('by {channel}', { channel: task.channel_name })}</span>
        </div>
      ) : null}
    </article>
  )
}

export default function Music() {
  const toast = useToast()
  const [models, setModels] = useState<ModelInfo[]>([])
  const [tasks, setTasks] = useState<MusicTask[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [lyrics, setLyrics] = useState('')
  const [instrumental, setInstrumental] = useState(false)
  const [format, setFormat] = useState('mp3')

  const musicModels = useMemo(() => models.filter(isMusicModel), [models])
  const pending = tasks.some(
    (task) =>
      task.status === 'queued' ||
      task.status === 'generating' ||
      (task.status === 'succeeded' && !task.audio_ready && !task.error),
  )
  const canSubmit = Boolean(model) && Boolean(prompt.trim()) && (instrumental || Boolean(lyrics.trim()))

  useEffect(() => {
    let active = true
    api
      .models()
      .then((response) => {
        if (active) setModels(response.data)
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : t('Failed to load models'))
      })
    api
      .music()
      .then((response) => {
        if (active) {
          setTasks(response.items)
          setTotal(response.total)
        }
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : t('Failed to load music'))
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    setModel((previous) => {
      if (previous && musicModels.some((item) => item.id === previous)) return previous
      // Prefer the current recommended text-to-music model when it is present.
      const recommended = musicModels.find((item) => /^music-3\.0(?:-|$)/.test(item.id))
      return recommended?.id ?? musicModels[0]?.id ?? ''
    })
  }, [musicModels])

  const refreshTasks = useCallback(async () => {
    try {
      const response = await api.music()
      setTasks(response.items)
      setTotal(response.total)
    } catch {
      // Polling is best effort; the visible task list stays as-is.
    }
  }, [])

  useEffect(() => {
    if (!pending) return
    const timer = window.setInterval(() => {
      void refreshTasks()
    }, 4000)
    return () => window.clearInterval(timer)
  }, [pending, refreshTasks])

  const submit = async () => {
    if (!model) {
      toast.error(t('Add a MiniMax Music channel first'))
      return
    }
    if (!prompt.trim()) {
      toast.error(t('Describe the style and mood you want'))
      return
    }
    if (!instrumental && !lyrics.trim()) {
      toast.error(t('Add lyrics, or switch on instrumental'))
      return
    }
    setSubmitting(true)
    try {
      const task = await api.createMusic({
        model,
        prompt: prompt.trim(),
        lyrics: instrumental ? undefined : lyrics.trim(),
        instrumental,
        format,
      })
      setTasks((previous) => [task, ...previous])
      setTotal((previous) => previous + 1)
      toast.success(t('Music generation started'))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t('Failed to start music generation'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title={t('Music generation')}
        subtitle={t('Turn a style description and lyrics into a full song with MiniMax Music. Generation runs in the background; preview the audio and download it here when it finishes.')}
        actions={
          <button className="btn btn-ghost" onClick={() => void refreshTasks()} disabled={loading}>
            <Icon name="refresh" size={16} /> {t("Refresh")}
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> {t('Loading music generation…')}
        </div>
      ) : (
        <div className="music-layout">
          <section className="panel music-create">
            <div className="panel-head">
              <div>
                <h2>{t('Compose a song')}</h2>
                <p>{t('Describe the sound, then write or paste the lyrics.')}</p>
              </div>
            </div>
            {musicModels.length === 0 ? (
              <EmptyState
                icon="music"
                title={t('No music model')}
                description={t('Add a MiniMax Music channel with a text-to-music model such as music-3.0, then come back here to compose songs. Chat and video channels are not part of this workflow.')}
                action={
                  <Link className="btn btn-primary" to="/channels">
                    <Icon name="plus" size={16} /> {t("Add a channel")}
                  </Link>
                }
              />
            ) : (
              <form
                className="music-form"
                onSubmit={(event) => {
                  event.preventDefault()
                  void submit()
                }}
              >
                <FormField label={t('Model')}>
                  <select className="input" value={model} onChange={(event) => setModel(event.target.value)}>
                    {musicModels.map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.id}
                      </option>
                    ))}
                  </select>
                </FormField>
                <FormField
                  label={t('Style description')}
                  hint={t('{n} / {max} characters — genre, mood, instruments and setting.', {
                    n: prompt.length,
                    max: MAX_PROMPT,
                  })}
                >
                  <textarea
                    className="input textarea"
                    rows={4}
                    maxLength={MAX_PROMPT}
                    value={prompt}
                    onChange={(event) => setPrompt(event.target.value)}
                    placeholder={t('Indie folk, melancholic, introspective, rainy night, fingerpicked guitar…')}
                  />
                </FormField>
                <FormField
                  label={t('Lyrics')}
                  hint={
                    instrumental
                      ? t('Instrumental songs do not use lyrics.')
                      : t('{n} / {max} characters — structure tags like [verse] and [chorus] are supported.', {
                          n: lyrics.length,
                          max: MAX_LYRICS,
                        })
                  }
                >
                  <textarea
                    className="input textarea mono"
                    rows={9}
                    maxLength={MAX_LYRICS}
                    value={lyrics}
                    disabled={instrumental}
                    onChange={(event) => setLyrics(event.target.value)}
                    placeholder={t('[verse]\n街灯闪烁，夜风轻叹\n\n[chorus]\n带我穿过雨幕回家')}
                  />
                </FormField>
                <label className="checkbox">
                  <input
                    type="checkbox"
                    checked={instrumental}
                    onChange={(event) => setInstrumental(event.target.checked)}
                  />
                  {t('Instrumental (no vocals)')}
                </label>
                <FormField label={t('Download format')}>
                  <select className="input" value={format} onChange={(event) => setFormat(event.target.value)}>
                    <option value="mp3">MP3</option>
                    <option value="wav">WAV</option>
                  </select>
                </FormField>
                <button className="btn btn-primary btn-block" type="submit" disabled={submitting || !canSubmit}>
                  {submitting ? <Spinner size={16} /> : <Icon name="music" size={16} />}
                  {t('Generate music')}
                </button>
                <span className="field-hint">
                  {t('A song usually takes a few minutes. You can leave this page; generation keeps running.')}
                </span>
              </form>
            )}
          </section>

          <section className="panel music-history">
            <div className="panel-head">
              <div>
                <h2>{t('Recent songs')}</h2>
                <p>
                  {total === 0
                    ? t('No songs yet')
                    : pending
                      ? t('{total} songs in this workspace · updating automatically', { total })
                      : t('{total} songs in this workspace', { total })}
                </p>
              </div>
              {pending ? <Spinner size={16} /> : null}
            </div>
            {tasks.length === 0 ? (
              <EmptyState
                icon="music"
                title={t('No songs yet')}
                description={t('Describe a style and add lyrics to compose your first song. Previews and downloads appear here when a song is ready.')}
              />
            ) : (
              <div className="music-list">
                {tasks.map((task) => (
                  <MusicCard key={task.id} task={task} />
                ))}
              </div>
            )}
          </section>
        </div>
      )}
    </div>
  )
}
