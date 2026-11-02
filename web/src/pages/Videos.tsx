import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ChangeEvent } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { EmptyState, ErrorBanner, FormField, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { relativeTime } from '../lib/format'
import { useToast } from '../lib/toast'
import type { ModelInfo, VideoTask, VideoTaskStatus } from '../lib/types'

const RATIOS = ['16:9', '4:3', '1:1', '3:4', '9:16', '21:9']
const MAX_PROMPT = 7000
const MAX_FRAME_BYTES = 30 * 1024 * 1024

function resolutionOptionsFor(model: string): string[] {
  if (!model || model === 'MiniMax-H3') return ['768P', '2K']
  if (model.includes('Highspeed') || model.includes('Lite') || model.includes('Max')) return ['480P', '768P']
  return ['768P', '2K']
}

function statusLabel(status: VideoTaskStatus): string {
  switch (status) {
    case 'queued':
      return 'Queued'
    case 'running':
      return 'Generating'
    case 'succeeded':
      return 'Succeeded'
    case 'failed':
      return 'Failed'
    case 'cancelled':
      return 'Cancelled'
  }
}

function VideoStatus({ status }: { status: VideoTaskStatus }) {
  const tone =
    status === 'succeeded' ? 'success' : status === 'failed' || status === 'cancelled' ? 'danger' : 'muted'
  return (
    <span className={`badge badge-${tone}`}>
      <span className={`badge-dot badge-dot-${tone}`} />
      {statusLabel(status)}
    </span>
  )
}

function modeLabel(task: VideoTask): string {
  return task.mode === 'first_frame' ? 'First frame' : 'Text to video'
}

function VideoCard({ task }: { task: VideoTask }) {
  const ready = task.video_ready && task.content_url
  return (
    <article className="video-card">
      {ready ? (
        <video className="video-preview" controls preload="metadata" playsInline src={task.content_url}>
          Your browser cannot play this video.
        </video>
      ) : (
        <div className={`video-preview video-placeholder state-${task.status}`}>
          {task.status === 'failed' || task.status === 'cancelled' ? (
            <Icon name="alert" size={22} />
          ) : task.status === 'succeeded' ? (
            <Icon name="download" size={22} />
          ) : (
            <Spinner size={22} />
          )}
          <span>
            {task.status === 'failed' || task.status === 'cancelled'
              ? 'Generation failed'
              : task.status === 'succeeded'
                ? 'Video is being downloaded'
                : 'Generating video…'}
          </span>
        </div>
      )}
      <div className="video-card-head">
        <div>
          <div className="video-model">{task.model}</div>
          <div className="video-meta">
            {modeLabel(task)} · {task.resolution} · {task.duration}s · {task.ratio} · {relativeTime(task.created_at)}
          </div>
        </div>
        <VideoStatus status={task.status} />
      </div>
      <p className="video-prompt">{task.prompt}</p>
      {task.error ? (
        <div className="video-error">
          <Icon name="alert" size={14} />
          <span>{task.error}</span>
        </div>
      ) : null}
      {ready ? (
        <div className="video-actions">
          <a className="btn btn-sm btn-ghost" href={`${task.content_url}?download=1`}>
            <Icon name="download" size={15} /> Download MP4
          </a>
          <span className="video-meta">by {task.channel_name}</span>
        </div>
      ) : null}
    </article>
  )
}

export default function Videos() {
  const toast = useToast()
  const [models, setModels] = useState<ModelInfo[]>([])
  const [tasks, setTasks] = useState<VideoTask[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [resolution, setResolution] = useState('768P')
  const [duration, setDuration] = useState(5)
  const [ratio, setRatio] = useState('16:9')
  const [firstFrame, setFirstFrame] = useState('')
  const [frameName, setFrameName] = useState('')

  const videoModels = useMemo(() => models.filter((item) => item.types.includes('minimax')), [models])
  const resolutionOptions = resolutionOptionsFor(model)
  const pending = tasks.some(
    (task) =>
      task.status === 'queued' ||
      task.status === 'running' ||
      (task.status === 'succeeded' && !task.video_ready && !task.error),
  )

  useEffect(() => {
    let active = true
    api
      .models()
      .then((response) => {
        if (active) setModels(response.data)
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : 'Failed to load models')
      })
    api
      .videos()
      .then((response) => {
        if (active) {
          setTasks(response.items)
          setTotal(response.total)
        }
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : 'Failed to load videos')
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    setModel((previous) =>
      previous && videoModels.some((item) => item.id === previous) ? previous : (videoModels[0]?.id ?? ''),
    )
  }, [videoModels])

  useEffect(() => {
    setResolution((previous) => (resolutionOptionsFor(model).includes(previous) ? previous : resolutionOptionsFor(model)[0]))
  }, [model])

  const refreshTasks = useCallback(async () => {
    try {
      const response = await api.videos()
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

  const pickFrame = (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    if (!file.type.startsWith('image/')) {
      toast.error('Choose an image file for the first frame')
      return
    }
    if (file.size > MAX_FRAME_BYTES) {
      toast.error('The first frame must be 30 MB or smaller')
      return
    }
    const reader = new FileReader()
    reader.onload = () => {
      if (typeof reader.result !== 'string') return
      setFirstFrame(reader.result)
      setFrameName(file.name)
      setRatio('adaptive')
    }
    reader.onerror = () => toast.error('Could not read that image')
    reader.readAsDataURL(file)
  }

  const removeFrame = () => {
    setFirstFrame('')
    setFrameName('')
    setRatio('16:9')
  }

  const submit = async () => {
    if (!model) {
      toast.error('Add a MiniMax video channel first')
      return
    }
    if (!prompt.trim()) {
      toast.error('Describe the video you want to generate')
      return
    }
    setSubmitting(true)
    try {
      const task = await api.createVideo({
        model,
        prompt: prompt.trim(),
        first_frame: firstFrame || undefined,
        resolution,
        duration,
        ratio: firstFrame ? 'adaptive' : ratio,
      })
      setTasks((previous) => [task, ...previous])
      setTotal((previous) => previous + 1)
      setPrompt('')
      removeFrame()
      toast.success('Video generation started')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to start video generation')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="page">
      <PageHeader
        title="Video generation"
        subtitle="MiniMax-H3 text-to-video and first-frame image-to-video. Generation runs asynchronously; previews and downloads appear here when a task finishes."
        actions={
          <button className="btn btn-ghost" onClick={() => void refreshTasks()} disabled={loading}>
            <Icon name="refresh" size={16} /> Refresh
          </button>
        }
      />
      {error ? <ErrorBanner message={error} /> : null}

      {loading ? (
        <div className="page-loading">
          <Spinner size={22} /> Loading video generation…
        </div>
      ) : (
        <div className="video-layout">
          <section className="panel video-create">
            <div className="panel-head">
              <div>
                <h2>Create a video</h2>
                <p>Describe a shot, or bring a first-frame image to life.</p>
              </div>
            </div>
            {videoModels.length === 0 ? (
              <EmptyState
                icon="video"
                title="No MiniMax video channel"
                description="Add a channel with the MiniMax H3 protocol and a MiniMax-H3 model, then come back here to generate videos."
                action={
                  <Link className="btn btn-primary" to="/channels">
                    <Icon name="plus" size={16} /> Add a channel
                  </Link>
                }
              />
            ) : (
              <form
                className="video-form"
                onSubmit={(event) => {
                  event.preventDefault()
                  void submit()
                }}
              >
                <FormField label="Model">
                  <select className="input" value={model} onChange={(event) => setModel(event.target.value)}>
                    {videoModels.map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.id}
                      </option>
                    ))}
                  </select>
                </FormField>
                <FormField label="Prompt" hint={`${prompt.length} / ${MAX_PROMPT} characters`}>
                  <textarea
                    className="input textarea"
                    rows={5}
                    maxLength={MAX_PROMPT}
                    value={prompt}
                    onChange={(event) => setPrompt(event.target.value)}
                    placeholder="A lighthouse on a stormy coast, waves crashing, slow cinematic push-in, dusk light…"
                  />
                </FormField>
                <div className="video-form-row">
                  <FormField label="Resolution">
                    <select className="input" value={resolution} onChange={(event) => setResolution(event.target.value)}>
                      {resolutionOptions.map((option) => (
                        <option key={option} value={option}>
                          {option}
                        </option>
                      ))}
                    </select>
                  </FormField>
                  <FormField label="Duration">
                    <select
                      className="input"
                      value={duration}
                      onChange={(event) => setDuration(Number.parseInt(event.target.value, 10))}
                    >
                      {Array.from({ length: 12 }, (_, index) => index + 4).map((seconds) => (
                        <option key={seconds} value={seconds}>
                          {seconds} seconds
                        </option>
                      ))}
                    </select>
                  </FormField>
                </div>
                <FormField
                  label="Aspect ratio"
                  hint={
                    firstFrame
                      ? 'Image-to-video adapts to the first frame.'
                      : 'Required for text-to-video.'
                  }
                >
                  <select
                    className="input"
                    value={firstFrame ? 'adaptive' : ratio}
                    disabled={Boolean(firstFrame)}
                    onChange={(event) => setRatio(event.target.value)}
                  >
                    {firstFrame ? <option value="adaptive">Adaptive (from first frame)</option> : null}
                    {RATIOS.map((option) => (
                      <option key={option} value={option}>
                        {option}
                      </option>
                    ))}
                  </select>
                </FormField>
                <div className="field">
                  <span className="field-label">First frame (optional)</span>
                  {firstFrame ? (
                    <div className="frame-preview">
                      <img src={firstFrame} alt="Selected first frame" />
                      <div className="frame-preview-foot">
                        <span className="mono">{frameName}</span>
                        <button type="button" className="btn btn-sm btn-ghost" onClick={removeFrame}>
                          <Icon name="x" size={14} /> Remove
                        </button>
                      </div>
                    </div>
                  ) : (
                    <label className="frame-drop">
                      <Icon name="upload" size={18} />
                      <span>Choose an image</span>
                      <input
                        type="file"
                        accept="image/png,image/jpeg,image/webp,image/heic,image/heif"
                        hidden
                        onChange={pickFrame}
                      />
                    </label>
                  )}
                  <span className="field-hint">
                    PNG, JPEG, WEBP, HEIC or HEIF up to 30 MB. Leave empty for text-to-video.
                  </span>
                </div>
                <button className="btn btn-primary btn-block" type="submit" disabled={submitting || !prompt.trim()}>
                  {submitting ? <Spinner size={16} /> : <Icon name="video" size={16} />}
                  Generate video
                </button>
                <span className="field-hint">
                  Video generation usually takes a few minutes. You can leave this page; the task keeps running.
                </span>
              </form>
            )}
          </section>

          <section className="panel video-history">
            <div className="panel-head">
              <div>
                <h2>Recent videos</h2>
                <p>
                  {total === 0 ? 'No videos yet' : `${total} task${total === 1 ? '' : 's'} in this workspace`}
                  {pending ? ' · updating automatically' : ''}
                </p>
              </div>
              {pending ? <Spinner size={16} /> : null}
            </div>
            {tasks.length === 0 ? (
              <EmptyState
                icon="video"
                title="No videos yet"
                description="Submit a prompt to generate your first MiniMax-H3 video. Text-to-video and first-frame image-to-video are both supported."
              />
            ) : (
              <div className="video-list">
                {tasks.map((task) => (
                  <VideoCard key={task.id} task={task} />
                ))}
              </div>
            )}
          </section>
        </div>
      )}
    </div>
  )
}
