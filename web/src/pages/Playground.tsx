import { useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { EmptyState, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import { GatewayRequestError, streamChatCompletion } from '../lib/gatewayChat'
import type { ChatUsage } from '../lib/gatewayChat'
import { formatCost, formatDuration, formatNumber, formatPrice, formatTime } from '../lib/format'
import { t } from '../lib/i18n'
import { useToast } from '../lib/toast'
import type { ModelInfo, Token } from '../lib/types'

type MessageRole = 'user' | 'assistant'
type MessageStatus = 'streaming' | 'complete' | 'stopped' | 'error'

interface RequestSnapshot {
  model: string
  keyName: string
  temperature: number
  maxTokens: number | null
}

interface ChatMessage {
  id: string
  role: MessageRole
  content: string
  status: MessageStatus
  requestId?: string
  latencyMs?: number
  usage?: ChatUsage
  error?: string
  request?: RequestSnapshot
  createdAt: number
}

type ConfirmState = { kind: 'retry'; messageId: string } | { kind: 'clear' } | null

const maxOutputTokens = 32768
const defaultTemperature = 0.7
const temperatureStep = 0.1

let messageSequence = 0
function nextMessageId(): string {
  messageSequence += 1
  return `msg-${Date.now()}-${messageSequence}`
}

function normalizedTemperature(value: number, max: number): number {
  return Math.min(max, Math.max(0, Math.round(value * 10) / 10))
}

function toApiMessages(history: ChatMessage[], systemPrompt: string) {
  const messages: { role: string; content: string }[] = []
  const system = systemPrompt.trim()
  if (system) messages.push({ role: 'system', content: system })
  for (const message of history) {
    if (!message.content.trim()) continue
    messages.push({ role: message.role, content: message.content })
  }
  return messages
}

function padDatePart(value: number): string {
  return String(value).padStart(2, '0')
}

function exportFileStem(date: Date): string {
  return `${date.getFullYear()}${padDatePart(date.getMonth() + 1)}${padDatePart(date.getDate())}-${padDatePart(date.getHours())}${padDatePart(date.getMinutes())}`
}

function exportTimestamp(date: Date): string {
  return `${date.getFullYear()}-${padDatePart(date.getMonth() + 1)}-${padDatePart(date.getDate())} ${padDatePart(date.getHours())}:${padDatePart(date.getMinutes())}`
}

function conversationMarkdown(messages: ChatMessage[], exportedAt: Date): string {
  const sections: string[] = [
    `# ${t('Playground conversation')}`,
    '',
    `_${t('Exported {time} (local time).', { time: exportTimestamp(exportedAt) })}_`,
  ]
  for (const message of messages) {
    const content = message.content.trim()
    if (!content) continue
    const role =
      message.role === 'user'
        ? t('User')
        : message.request
          ? t('Assistant ({model})', { model: message.request.model })
          : t('Assistant')
    sections.push('', `## ${role}`, '', content)
    if (message.status === 'stopped') {
      sections.push('', `> _${t('Generation stopped before completion.')}_`)
    } else if (message.status === 'error') {
      const detail = message.error ? message.error.replace(/\s+/g, ' ').trim() : ''
      sections.push(
        '',
        detail ? `> _${t('Request failed: {detail}.', { detail })}_` : `> _${t('Request failed.')}_`,
      )
    }
  }
  return `${sections.join('\n')}\n`
}

function downloadMarkdown(filename: string, content: string) {
  const blob = new Blob([content], { type: 'text/markdown;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.append(link)
  link.click()
  link.remove()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

export default function Playground() {
  const toast = useToast()
  const [catalog, setCatalog] = useState<ModelInfo[]>([])
  const [catalogError, setCatalogError] = useState('')
  const [keys, setKeys] = useState<Token[]>([])
  const [keysLoading, setKeysLoading] = useState(true)
  const [keysError, setKeysError] = useState('')
  const [selectedKeyId, setSelectedKeyId] = useState<number | null>(null)
  const [models, setModels] = useState<string[]>([])
  const [modelsLoading, setModelsLoading] = useState(false)
  const [modelsError, setModelsError] = useState('')
  const [modelsRefresh, setModelsRefresh] = useState(0)
  const [selectedModel, setSelectedModel] = useState('')
  const [systemPrompt, setSystemPrompt] = useState('')
  const [temperature, setTemperature] = useState(defaultTemperature)
  const [maxTokens, setMaxTokens] = useState('')
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState('')
  const [sending, setSending] = useState(false)
  const [confirm, setConfirm] = useState<ConfirmState>(null)
  const abortRef = useRef<AbortController | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)

  const loadKeys = async () => {
    setKeysLoading(true)
    try {
      const response = await api.tokens()
      setKeys(response.data)
      setKeysError('')
      setModelsRefresh((value) => value + 1)
      setSelectedKeyId((current) => {
        if (current !== null && response.data.some((key) => key.id === current)) return current
        const now = Math.floor(Date.now() / 1000)
        const usable = response.data.find(
          (key) =>
            key.status === 'active' &&
            (key.quota < 0 || key.used_quota < key.quota) &&
            (key.expires_at === null || key.expires_at > now),
        )
        return usable?.id ?? response.data.find((key) => key.status === 'active')?.id ?? response.data[0]?.id ?? null
      })
    } catch (error) {
      setKeysError(error instanceof Error ? error.message : t('Failed to load gateway keys'))
    } finally {
      setKeysLoading(false)
    }
  }

  useEffect(() => {
    void loadKeys()
  }, [])

  useEffect(() => {
    let active = true
    api
      .models()
      .then((response) => {
        if (!active) return
        setCatalog(response.data)
        setCatalogError('')
      })
      .catch((error: unknown) => {
        if (active) setCatalogError(error instanceof Error ? error.message : t('Failed to load the model catalog'))
      })
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    const element = scrollRef.current
    if (element) element.scrollTop = element.scrollHeight
  }, [messages])

  useEffect(() => {
    let active = true
    if (selectedKeyId === null) {
      setModels([])
      setSelectedModel('')
      setModelsError('')
      return
    }
    setModelsLoading(true)
    setModelsError('')
    api
      .playgroundModels(selectedKeyId)
      .then((response) => {
        if (!active) return
        const available = response.data
          .map((model) => model.id)
          .filter((id): id is string => typeof id === 'string' && id !== '')
        setModels(available)
        setSelectedModel((current) => (available.includes(current) ? current : (available[0] ?? '')))
      })
      .catch((error: unknown) => {
        if (!active) return
        setModels([])
        setSelectedModel('')
        setModelsError(error instanceof Error ? error.message : t('Failed to load models for this key'))
      })
      .finally(() => {
        if (active) setModelsLoading(false)
      })
    return () => {
      active = false
    }
  }, [selectedKeyId, modelsRefresh])

  const selectedKey = useMemo(
    () => keys.find((key) => key.id === selectedKeyId) ?? null,
    [keys, selectedKeyId],
  )
  const keyExpired =
    selectedKey?.expires_at !== null &&
    selectedKey?.expires_at !== undefined &&
    selectedKey.expires_at <= Math.floor(Date.now() / 1000)
  const keyUsable = selectedKey?.status === 'active' && !keyExpired

  const selectedInfo = useMemo(
    () => catalog.find((model) => model.id === selectedModel),
    [catalog, selectedModel],
  )
  const anthropicServed = selectedInfo?.types.includes('anthropic') ?? false
  const temperatureMax = anthropicServed ? 1 : 2

  useEffect(() => {
    setTemperature((current) => normalizedTemperature(current, temperatureMax))
  }, [temperatureMax])

  const maxTokensValue = useMemo(() => {
    const trimmed = maxTokens.trim()
    if (!trimmed) return null
    const value = Number(trimmed)
    if (!Number.isInteger(value) || value < 1 || value > maxOutputTokens) return null
    return value
  }, [maxTokens])
  const maxTokensInvalid = maxTokens.trim() !== '' && maxTokensValue === null

  const modelReady = keyUsable && models.length > 0 && selectedModel !== '' && !modelsLoading
  const canType = modelReady && !sending
  const canSend = canType && input.trim() !== '' && !maxTokensInvalid
  const canRetry = modelReady && !maxTokensInvalid && !sending
  const canExport = !sending && messages.some((message) => message.content.trim() !== '')
  const turns = messages.filter((message) => message.role === 'user').length

  const temperatureHint = useMemo(() => {
    if (!selectedInfo) {
      return t('Range 0–2 for OpenAI-compatible models. Select a model to see its supported range.')
    }
    if (anthropicServed && selectedInfo.types.includes('openai')) {
      return t(
        'This model is served by both Anthropic and OpenAI channels; the temperature cap is 1 for compatibility.',
      )
    }
    if (anthropicServed) {
      return t('This model is served by an Anthropic channel, whose temperature range is 0–1.')
    }
    return t('This model is served by OpenAI-compatible channels, whose temperature range is 0–2.')
  }, [anthropicServed, selectedInfo])

  const emptyModelMessage = useMemo(() => {
    if (modelsError) return modelsError
    if (keysError) return keysError
    if (!keysLoading && keys.length === 0) {
      return t('No gateway keys yet. Create one on the API keys page; the key secret stays on the server.')
    }
    if (!keyUsable) {
      return keyExpired
        ? t('This gateway key has expired. Select another key.')
        : t('Select an active gateway key to load models.')
    }
    if (catalogError) {
      return t(
        'The relay reported no available models for this key. Check the key allowlist and enabled channels.',
      )
    }
    if (catalog.length === 0) {
      return t('The gateway has no available models. Add and enable a channel in Channels first.')
    }
    return t(
      'This key has no available models: its model allowlist may not match any enabled channel, or the channels are disabled.',
    )
  }, [modelsError, keysError, keysLoading, keys.length, keyUsable, keyExpired, catalog.length, catalogError])

  const patchAssistant = (id: string, patch: Partial<ChatMessage>) => {
    setMessages((current) => current.map((message) => (message.id === id ? { ...message, ...patch } : message)))
  }

  const runCompletion = async (history: ChatMessage[]) => {
    if (selectedKeyId === null || selectedKey === null) {
      setKeysError(t('Select a gateway key first.'))
      return
    }
    if (!selectedModel) return

    const request: RequestSnapshot = {
      model: selectedModel,
      keyName: selectedKey.name,
      temperature,
      maxTokens: maxTokensValue,
    }
    const assistantId = nextMessageId()
    const placeholder: ChatMessage = {
      id: assistantId,
      role: 'assistant',
      content: '',
      status: 'streaming',
      request,
      createdAt: Date.now(),
    }
    setMessages([...history, placeholder])
    setSending(true)

    const controller = new AbortController()
    abortRef.current = controller
    const started = performance.now()
    let received = ''
    let requestId: string | undefined
    let latencyMs: number | undefined
    let usage: ChatUsage | undefined

    try {
      const body: Record<string, unknown> = {
        model: request.model,
        messages: toApiMessages(history, systemPrompt),
        stream: true,
        stream_options: { include_usage: true },
        temperature: request.temperature,
      }
      if (request.maxTokens !== null) body.max_tokens = request.maxTokens

      const result = await streamChatCompletion({
        tokenId: selectedKeyId,
        body,
        signal: controller.signal,
        onDelta: (content) => {
          received = content
          patchAssistant(assistantId, { content })
        },
      })
      requestId = result.requestId
      usage = result.usage
      latencyMs = performance.now() - started

      if (result.aborted) {
        patchAssistant(assistantId, {
          content: result.content,
          status: 'stopped',
          requestId,
          usage,
          latencyMs,
        })
        return
      }
      if (result.error) {
        patchAssistant(assistantId, {
          content: result.content,
          status: 'error',
          error: result.error,
          requestId,
          usage,
          latencyMs,
        })
        return
      }
      patchAssistant(assistantId, {
        content: result.content,
        status: 'complete',
        requestId,
        usage,
        latencyMs,
      })
    } catch (error) {
      latencyMs = performance.now() - started
      if (controller.signal.aborted) {
        patchAssistant(assistantId, {
          content: received,
          status: 'stopped',
          requestId,
          usage,
          latencyMs,
        })
        return
      }
      if (error instanceof GatewayRequestError) {
        patchAssistant(assistantId, {
          content: received,
          status: 'error',
          error: error.message,
          requestId: error.requestId ?? requestId,
          usage,
          latencyMs,
        })
        return
      }
      patchAssistant(assistantId, {
        content: received,
        status: 'error',
        error: t('Request failed: {error}', {
          error: error instanceof Error ? error.message : String(error),
        }),
        requestId,
        usage,
        latencyMs,
      })
    } finally {
      abortRef.current = null
      setSending(false)
    }
  }

  const sendMessage = () => {
    if (!canSend) return
    const text = input.trim()
    if (!text) return
    const userMessage: ChatMessage = {
      id: nextMessageId(),
      role: 'user',
      content: text,
      status: 'complete',
      createdAt: Date.now(),
    }
    const history = [...messages, userMessage]
    setMessages(history)
    setInput('')
    void runCompletion(history)
  }

  const stopGeneration = () => {
    abortRef.current?.abort()
  }

  const handleComposerKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      sendMessage()
    }
  }

  const copyRequestId = async (requestId: string) => {
    try {
      await navigator.clipboard.writeText(requestId)
      toast.success(t('Request ID copied'))
    } catch {
      toast.error(t('Cannot access the clipboard; select and copy the value manually'))
    }
  }

  const retryMessage = confirm?.kind === 'retry'
    ? messages.find((message) => message.id === confirm.messageId)
    : undefined

  const confirmRetry = () => {
    if (!confirm || confirm.kind !== 'retry' || !canRetry) return
    const index = messages.findIndex((message) => message.id === confirm.messageId)
    if (index <= 0) {
      setConfirm(null)
      return
    }
    const history = messages.slice(0, index)
    setMessages(history)
    setConfirm(null)
    void runCompletion(history)
  }

  const confirmClear = () => {
    setMessages([])
    setConfirm(null)
    toast.success(t('Conversation cleared'))
  }

  const exportConversation = () => {
    if (!canExport) return
    const exportedAt = new Date()
    const filename = `playground-conversation-${exportFileStem(exportedAt)}.md`
    downloadMarkdown(filename, conversationMarkdown(messages, exportedAt))
    toast.success(t('Conversation exported as Markdown'))
  }

  const keyBadge = keysLoading ? (
    <span className="badge badge-muted"><span className="badge-dot badge-dot-muted" />{t('Loading')}</span>
  ) : !selectedKey ? (
    <span className="badge badge-muted"><span className="badge-dot badge-dot-muted" />{t('No key')}</span>
  ) : keyExpired ? (
    <span className="badge badge-danger"><span className="badge-dot badge-dot-danger" />{t('Expired')}</span>
  ) : selectedKey.status === 'active' ? (
    <span className="badge badge-success"><span className="badge-dot badge-dot-success" />{t('Active')}</span>
  ) : (
    <span className="badge badge-danger"><span className="badge-dot badge-dot-danger" />{t('Disabled')}</span>
  )

  return (
    <div className="page playground-page">
      <PageHeader
        title={t('Playground')}
        subtitle={t(
          "Pick an existing gateway key, choose a model and chat. The relay applies that key's model access, quota, rate limit and logs; the key secret never reaches the browser.",
        )}
        actions={
          <>
            <button
              className="btn btn-ghost"
              onClick={exportConversation}
              disabled={!canExport}
              title={t('Download this conversation as a Markdown file')}
            >
              <Icon name="download" size={15} /> {t('Export Markdown')}
            </button>
            <button
              className="btn btn-ghost"
              onClick={() => setConfirm({ kind: 'clear' })}
              disabled={messages.length === 0 || sending}
            >
              <Icon name="trash" size={15} /> {t('Clear conversation')}
            </button>
          </>
        }
      />

      <div className="playground-grid">
        <section className="panel chat-panel">
          <div className="chat-head">
            <div className="chat-head-title">
              <Icon name="spark" size={16} /> {t('Conversation')}
            </div>
            <div className="chat-head-meta">
              {sending ? (
                <>
                  <Spinner size={13} /> {t('Generating…')}
                </>
              ) : messages.length > 0 ? (
                t(
                  turns === 1
                    ? messages.length === 1
                      ? "{turns} turn · {messages} message"
                      : "{turns} turn · {messages} messages"
                    : messages.length === 1
                      ? "{turns} turns · {messages} message"
                      : "{turns} turns · {messages} messages",
                  { turns, messages: messages.length },
                )
              ) : (
                t('Not started')
              )}
            </div>
          </div>

          <div className="chat-scroll" ref={scrollRef}>
            {!modelReady || messages.length === 0 ? (
              <div className="chat-empty-wrap">
                {keysLoading ? (
                  <div className="page-loading">
                    <Spinner size={18} /> {t('Loading gateway keys…')}
                  </div>
                ) : keysError ? (
                  <EmptyState
                    icon="alert"
                    title={t('Could not load gateway keys')}
                    description={keysError}
                    action={
                      <button className="btn btn-ghost" onClick={() => void loadKeys()}>
                        <Icon name="refresh" size={15} /> {t('Retry')}
                      </button>
                    }
                  />
                ) : !selectedKey ? (
                  <EmptyState
                    icon="key"
                    title={t('Create a gateway key first')}
                    description={t(
                      'Playground uses an existing key from API keys. The relay uses it server-side; the key secret is never sent to the browser.',
                    )}
                    action={
                      <Link className="btn btn-primary" to="/tokens">
                        <Icon name="key" size={15} /> {t('Go to API keys')}
                      </Link>
                    }
                  />
                ) : !keyUsable ? (
                  <EmptyState
                    icon="alert"
                    title={keyExpired ? t('This key has expired') : t('This key is disabled')}
                    description={t(
                      'Select an active gateway key in the panel on the right, or update this key on the API keys page.',
                    )}
                  />
                ) : modelsLoading ? (
                  <div className="page-loading">
                    <Spinner size={18} /> {t('Loading models for this key…')}
                  </div>
                ) : !modelReady ? (
                  <EmptyState
                    icon={modelsError ? 'alert' : 'models'}
                    title={modelsError ? t('Could not load models') : t('No models available for this key')}
                    description={emptyModelMessage}
                    action={
                      <button className="btn btn-ghost" onClick={() => setModelsRefresh((value) => value + 1)}>
                        <Icon name="refresh" size={15} /> {t('Retry')}
                      </button>
                    }
                  />
                ) : (
                  <EmptyState
                    icon="spark"
                    title={t('Start a real request')}
                    description={t(
                      'Pick a model and send a message. Responses stream in, keep multi-turn context, can be stopped, and can be traced in Logs by request ID.',
                    )}
                  />
                )}
              </div>
            ) : (
              messages.map((message, index) => {
                const isLast = index === messages.length - 1
                const roleLabel = message.role === 'user' ? t('You') : message.request?.model || t('Assistant')
                return (
                  <article className={`chat-message ${message.role === 'user' ? 'from-user' : 'from-assistant'}`} key={message.id}>
                    <div className="chat-avatar">{message.role === 'user' ? t('You') : t('AI')}</div>
                    <div className="chat-body">
                      <div className="chat-role">{roleLabel}</div>
                      {message.content ? (
                        <div className="chat-content">
                          {message.content}
                          {message.status === 'streaming' ? <span className="chat-caret" aria-hidden="true" /> : null}
                        </div>
                      ) : message.status === 'streaming' ? (
                        <div className="chat-thinking">
                          <Spinner size={13} /> {t('Waiting for the first chunk…')}
                        </div>
                      ) : null}

                      {message.error ? (
                        <div className="chat-error" role="alert">
                          <Icon name="alert" size={15} />
                          <span>{message.error}</span>
                        </div>
                      ) : null}

                      {message.status === 'stopped' ? (
                        <div className="chat-stopped-note">
                          {t('Generation stopped. Content already received may still be billed.')}
                        </div>
                      ) : null}

                      {message.role === 'assistant' && message.status !== 'streaming' ? (
                        <div className="chat-meta">
                          {message.request ? (
                            <span>
                              {t('key {key} · {model} · temp {temperature} · max output {maxTokens}', {
                                key: message.request.keyName,
                                model: message.request.model,
                                temperature: message.request.temperature.toFixed(1),
                                maxTokens: message.request.maxTokens ?? t('default'),
                              })}
                            </span>
                          ) : null}
                          {message.latencyMs !== undefined ? (
                            <span>
                              <Icon name="clock" size={12} /> {formatDuration(Math.round(message.latencyMs))}
                            </span>
                          ) : null}
                          {message.usage ? (
                            <span
                              className={`usage-pill${message.usage.source === 'estimated' ? ' estimated' : ''}`}
                              title={
                                message.usage.source === 'estimated'
                                  ? t(
                                      'The upstream did not report usage; the relay estimated it from the request and returned text.',
                                    )
                                  : t('Token usage reported by the upstream.')
                              }
                            >
                              {message.usage.source === 'estimated'
                                ? t('Tokens {total} ({prompt} in / {completion} out) · estimated', {
                                    total: formatNumber(message.usage.total),
                                    prompt: formatNumber(message.usage.prompt),
                                    completion: formatNumber(message.usage.completion),
                                  })
                                : t('Tokens {total} ({prompt} in / {completion} out)', {
                                    total: formatNumber(message.usage.total),
                                    prompt: formatNumber(message.usage.prompt),
                                    completion: formatNumber(message.usage.completion),
                                  })}
                            </span>
                          ) : (
                            <span className="muted">{t('Token usage unavailable for this response')}</span>
                          )}
                          {message.requestId ? (
                            <span className="request-id">
                              {t('Request')} <code>{message.requestId}</code>
                              <button
                                className="link-button"
                                type="button"
                                onClick={() => void copyRequestId(message.requestId ?? '')}
                              >
                                {t('Copy')}
                              </button>
                              <Link className="link-button" to={`/logs?request_id=${encodeURIComponent(message.requestId)}`}>
                                {t('View log')}
                              </Link>
                            </span>
                          ) : null}
                        </div>
                      ) : null}

                      {message.role === 'assistant' && isLast && !sending ? (
                        <div className="chat-actions">
                          <button
                            className="btn btn-ghost btn-sm"
                            onClick={() => setConfirm({ kind: 'retry', messageId: message.id })}
                            disabled={!canRetry}
                            title={
                              maxTokensInvalid
                                ? t('Fix the max output value first')
                                : t('Creates a new gateway request and may incur new charges')
                            }
                          >
                            <Icon name="refresh" size={14} /> {t('Regenerate')}
                          </button>
                          <span className="chat-action-hint">
                            {t('Sends a new request and may incur new charges.')}
                          </span>
                        </div>
                      ) : null}
                    </div>
                  </article>
                )
              })
            )}
          </div>

          <div className="composer">
            <textarea
              className="composer-input"
              rows={3}
              value={input}
              onChange={(event) => setInput(event.target.value)}
              onKeyDown={handleComposerKeyDown}
              placeholder={
                modelReady
                  ? t('Type a message. Enter to send, Shift+Enter for a new line.')
                  : t('Select a gateway key and model above.')
              }
              disabled={!canType}
              aria-label={t('Message')}
            />
            <div className="composer-bar">
              <span className="composer-hint">
                {maxTokensInvalid
                  ? t('Max output must be an integer between 1 and {max}.', { max: maxOutputTokens })
                  : t('Requests go through this relay; this page never calls an upstream directly.')}
              </span>
              {sending ? (
                <button className="btn btn-danger" onClick={stopGeneration}>
                  <Icon name="stop" size={14} /> {t('Stop generating')}
                </button>
              ) : (
                <button className="btn btn-primary" onClick={sendMessage} disabled={!canSend}>
                  <Icon name="send" size={15} /> {t('Send')}
                </button>
              )}
            </div>
            <p className="composer-note">
              {t(
                "Every send or regeneration creates a new gateway request and may incur charges. Provider billing applies; the relay's cost display depends on configured channel prices. This page never retries automatically.",
              )}
            </p>
          </div>
        </section>

        <aside className="playground-side">
          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>{t('Gateway key')}</h2>
              <div className="key-head-actions">
                {keyBadge}
                <button
                  className="icon-button"
                  type="button"
                  onClick={() => void loadKeys()}
                  disabled={keysLoading || sending}
                  title={t('Refresh API keys')}
                  aria-label={t('Refresh gateway keys')}
                >
                  <Icon name="refresh" size={16} />
                </button>
              </div>
            </div>
            <label className="field">
              <span className="field-label">{t('Use an existing key')}</span>
              <select
                className="input"
                value={selectedKeyId ?? ''}
                onChange={(event) => setSelectedKeyId(event.target.value ? Number(event.target.value) : null)}
                disabled={keysLoading || sending || keys.length === 0}
                aria-label={t('Gateway key')}
              >
                {keys.length === 0 ? (
                  <option value="">{keysLoading ? t('Loading keys…') : t('No gateway keys')}</option>
                ) : (
                  keys.map((key) => (
                    <option value={key.id} key={key.id}>
                      {key.name} · {key.key_prefix} · {t(key.status.charAt(0).toUpperCase() + key.status.slice(1))}
                    </option>
                  ))
                )}
              </select>
              <span className="field-hint">
                {t(
                  'The relay uses this key server-side; the browser never sees its secret. Model access, quota, RPM limit and request logs apply.',
                )}
              </span>
            </label>
            {selectedKey ? (
              <div className="key-meta">
                <div>
                  <span>{t('Quota')}</span>
                  <strong>
                    {selectedKey.quota < 0
                      ? t('Unlimited')
                      : `${formatCost(selectedKey.used_quota)} / ${formatCost(selectedKey.quota)}`}
                  </strong>
                </div>
                <div>
                  <span>{t('Rate limit')}</span>
                  <strong>
                    {selectedKey.rpm_limit > 0
                      ? t('{rate} / min', { rate: selectedKey.rpm_limit })
                      : t('Unlimited')}
                  </strong>
                </div>
                <div>
                  <span>{t('Model access')}</span>
                  <strong>
                    {selectedKey.allowed_models.length > 0
                      ? t('{n} allowed', { n: selectedKey.allowed_models.length })
                      : t('All models')}
                  </strong>
                </div>
                <div>
                  <span>{t('Expires')}</span>
                  <strong>{selectedKey.expires_at ? formatTime(selectedKey.expires_at) : t('Never')}</strong>
                </div>
              </div>
            ) : null}
            {keysError ? (
              <div className="side-error" role="alert">
                <Icon name="alert" size={14} />
                <span>{keysError}</span>
              </div>
            ) : null}
            <p className="side-note">
              <Link to="/tokens">{t('Manage keys in API keys.')}</Link>
            </p>
          </section>

          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>{t('Model')}</h2>
              {modelsLoading ? (
                <Spinner size={13} />
              ) : modelReady ? (
                <span className="badge badge-muted">
                  {t('{n} available', { n: models.length })}
                </span>
              ) : null}
            </div>
            <select
              className="input"
              value={selectedModel}
              onChange={(event) => setSelectedModel(event.target.value)}
              disabled={!keyUsable || modelsLoading || sending}
              aria-label={t('Model')}
            >
              {modelsLoading ? (
                <option value="">{t('Loading models…')}</option>
              ) : models.length === 0 ? (
                <option value="">
                  {keyUsable ? t('No models available') : t('Select an active gateway key')}
                </option>
              ) : (
                models.map((model) => (
                  <option value={model} key={model}>
                    {model}
                  </option>
                ))
              )}
            </select>
            {modelsError ? (
              <div className="side-error" role="alert">
                <Icon name="alert" size={14} />
                <span>{modelsError}</span>
              </div>
            ) : null}
            {selectedInfo ? (
              <div className="model-detail">
                <div className="chip-row">
                  {selectedInfo.types.map((type) => (
                    <span className={`type-badge type-${type}`} key={type}>
                      {type === 'anthropic' ? 'Anthropic' : 'OpenAI'}
                    </span>
                  ))}
                  {selectedInfo.channels.map((channel) => (
                    <span className="chip" key={channel}>
                      {channel}
                    </span>
                  ))}
                </div>
                <div className="model-price">
                  {selectedInfo.input_price === 0 && selectedInfo.output_price === 0 ? (
                    <>{t('Cost price is not configured for this channel. Provider billing still applies.')}</>
                  ) : (
                    <>
                      {t('Input {input} / 1M · Output {output} / 1M', {
                        input: formatPrice(selectedInfo.input_price),
                        output: formatPrice(selectedInfo.output_price),
                      })}{' '}
                      <span className="muted">{t('(configured prices used for cost estimates)')}</span>
                    </>
                  )}
                </div>
              </div>
            ) : null}
            {catalogError ? (
              <div className="field-hint">
                {t('Failed to load the model catalog: {error}', { error: catalogError })}
              </div>
            ) : null}
            <p className="field-hint model-note">
              {t(
                "Available models come from the key's model allowlist and enabled channels. Switching models only affects the next request; the conversation is not resent automatically, and history stays in context.",
              )}
            </p>
          </section>

          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>{t('Chat parameters')}</h2>
            </div>
            <label className="field">
              <span className="field-label">{t('System prompt')}</span>
              <textarea
                className="input textarea"
                rows={3}
                value={systemPrompt}
                onChange={(event) => setSystemPrompt(event.target.value)}
                placeholder={t('Optional. For example: You are a concise technical assistant.')}
                disabled={sending}
                aria-label={t('System prompt')}
              />
              <span className="field-hint">{t('Changes apply from the next request.')}</span>
            </label>

            <div className="field">
              <div className="param-head">
                <span className="field-label">{t('Temperature')}</span>
                <span className="param-value">{temperature.toFixed(1)}</span>
              </div>
              <input
                className="param-range"
                type="range"
                min={0}
                max={temperatureMax}
                step={temperatureStep}
                value={temperature}
                onChange={(event) => setTemperature(normalizedTemperature(Number(event.target.value), temperatureMax))}
                disabled={sending}
                aria-label={t('Temperature')}
              />
              <span className="field-hint">{temperatureHint}</span>
            </div>

            <label className="field">
              <span className="field-label">{t('Max output tokens')}</span>
              <input
                className={`input${maxTokensInvalid ? ' input-invalid' : ''}`}
                type="number"
                min={1}
                max={maxOutputTokens}
                step={1}
                value={maxTokens}
                onChange={(event) => setMaxTokens(event.target.value)}
                placeholder={t('Leave empty for the default')}
                disabled={sending}
                aria-label={t('Maximum output tokens')}
              />
              <span className="field-hint">
                {t(
                  '1–{max}, or empty for the model/relay default. The exact model limit is enforced by the upstream; if exceeded, its error is shown here.',
                  { max: maxOutputTokens },
                )}
              </span>
            </label>

            <div className="param-info">
              <Icon name="shield" size={14} />
              <span>
                {t(
                  "Parameter ranges follow the selected model's channel protocol. If the model rejects a parameter, the upstream error is shown in the conversation.",
                )}
              </span>
            </div>
          </section>
        </aside>
      </div>

      <Modal
        open={confirm?.kind === 'retry'}
        onClose={() => setConfirm(null)}
        title={t('Send this turn again?')}
        subtitle={t('This creates a new gateway request and may incur new charges.')}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setConfirm(null)}>
              {t('Cancel')}
            </button>
            <button className="btn btn-primary" onClick={confirmRetry} disabled={!canRetry}>
              {t('Send new request')}
            </button>
          </>
        }
      >
        <p className="modal-copy">
          {t(
            "The last user message will be sent again and the current assistant reply will be replaced. The request uses the page's current settings:",
          )}
        </p>
        <div className="confirm-params">
          <div>
            <span>{t('Model')}</span>
            <strong className="mono">{selectedModel || "—"}</strong>
          </div>
          <div>
            <span>{t('Temperature')}</span>
            <strong>{temperature.toFixed(1)}</strong>
          </div>
          <div>
            <span>{t('Max output')}</span>
            <strong>{maxTokensValue ?? t('Default')}</strong>
          </div>
        </div>
        {retryMessage?.status === 'stopped' ? (
          <p className="modal-copy">
            {t('The previous generation was stopped; the new request starts from scratch.')}
          </p>
        ) : null}
        <p className="modal-copy">
          {t('Nothing is retried automatically. A request is sent only after you click “{action}”.', {
            action: t('Send new request'),
          })}
        </p>
      </Modal>

      <Modal
        open={confirm?.kind === 'clear'}
        onClose={() => setConfirm(null)}
        title={t('Clear this conversation?')}
        subtitle={t('The system prompt and parameters are kept.')}
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setConfirm(null)}>
              {t('Cancel')}
            </button>
            <button className="btn btn-danger" onClick={confirmClear}>
              {t('Clear conversation')}
            </button>
          </>
        }
      >
        <p className="modal-copy">
          {t(
            'All messages on this page will be removed. Existing request logs and charges remain available in Logs.',
          )}
        </p>
      </Modal>
    </div>
  )
}
