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
      setKeysError(error instanceof Error ? error.message : 'Failed to load gateway keys')
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
        if (active) setCatalogError(error instanceof Error ? error.message : 'Failed to load the model catalog')
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
        setModelsError(error instanceof Error ? error.message : 'Failed to load models for this key')
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
  const turns = messages.filter((message) => message.role === 'user').length

  const temperatureHint = useMemo(() => {
    if (!selectedInfo) return 'Range 0–2 for OpenAI-compatible models. Select a model to see its supported range.'
    if (anthropicServed && selectedInfo.types.includes('openai')) {
      return 'This model is served by both Anthropic and OpenAI channels; the temperature cap is 1 for compatibility.'
    }
    if (anthropicServed) return 'This model is served by an Anthropic channel, whose temperature range is 0–1.'
    return 'This model is served by OpenAI-compatible channels, whose temperature range is 0–2.'
  }, [anthropicServed, selectedInfo])

  const emptyModelMessage = useMemo(() => {
    if (modelsError) return modelsError
    if (keysError) return keysError
    if (!keysLoading && keys.length === 0) {
      return 'No gateway keys yet. Create one on the API keys page; the key secret stays on the server.'
    }
    if (!keyUsable) {
      return keyExpired ? 'This gateway key has expired. Select another key.' : 'Select an active gateway key to load models.'
    }
    if (catalogError) {
      return 'The relay reported no available models for this key. Check the key allowlist and enabled channels.'
    }
    if (catalog.length === 0) {
      return 'The gateway has no available models. Add and enable a channel in Channels first.'
    }
    return 'This key has no available models: its model allowlist may not match any enabled channel, or the channels are disabled.'
  }, [modelsError, keysError, keysLoading, keys.length, keyUsable, keyExpired, catalog.length, catalogError])

  const patchAssistant = (id: string, patch: Partial<ChatMessage>) => {
    setMessages((current) => current.map((message) => (message.id === id ? { ...message, ...patch } : message)))
  }

  const runCompletion = async (history: ChatMessage[]) => {
    if (selectedKeyId === null || selectedKey === null) {
      setKeysError('Select a gateway key first.')
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
        error: `Request failed: ${error instanceof Error ? error.message : String(error)}`,
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
      toast.success('Request ID copied')
    } catch {
      toast.error('Cannot access the clipboard; select and copy the value manually')
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
    toast.success('Conversation cleared')
  }

  const keyBadge = keysLoading ? (
    <span className="badge badge-muted"><span className="badge-dot badge-dot-muted" />Loading</span>
  ) : !selectedKey ? (
    <span className="badge badge-muted"><span className="badge-dot badge-dot-muted" />No key</span>
  ) : keyExpired ? (
    <span className="badge badge-danger"><span className="badge-dot badge-dot-danger" />Expired</span>
  ) : selectedKey.status === 'active' ? (
    <span className="badge badge-success"><span className="badge-dot badge-dot-success" />Active</span>
  ) : (
    <span className="badge badge-danger"><span className="badge-dot badge-dot-danger" />Disabled</span>
  )

  return (
    <div className="page playground-page">
      <PageHeader
        title="Playground"
        subtitle="Pick an existing gateway key, choose a model and chat. The relay applies that key's model access, quota, rate limit and logs; the key secret never reaches the browser."
        actions={
          <button
            className="btn btn-ghost"
            onClick={() => setConfirm({ kind: 'clear' })}
            disabled={messages.length === 0 || sending}
          >
            <Icon name="trash" size={15} /> Clear conversation
          </button>
        }
      />

      <div className="playground-grid">
        <section className="panel chat-panel">
          <div className="chat-head">
            <div className="chat-head-title">
              <Icon name="spark" size={16} /> Conversation
            </div>
            <div className="chat-head-meta">
              {sending ? (
                <>
                  <Spinner size={13} /> Generating…
                </>
              ) : messages.length > 0 ? (
                `${turns} turn${turns === 1 ? '' : 's'} · ${messages.length} message${messages.length === 1 ? '' : 's'}`
              ) : (
                'Not started'
              )}
            </div>
          </div>

          <div className="chat-scroll" ref={scrollRef}>
            {!modelReady || messages.length === 0 ? (
              <div className="chat-empty-wrap">
                {keysLoading ? (
                  <div className="page-loading">
                    <Spinner size={18} /> Loading gateway keys…
                  </div>
                ) : keysError ? (
                  <EmptyState
                    icon="alert"
                    title="Could not load gateway keys"
                    description={keysError}
                    action={
                      <button className="btn btn-ghost" onClick={() => void loadKeys()}>
                        <Icon name="refresh" size={15} /> Retry
                      </button>
                    }
                  />
                ) : !selectedKey ? (
                  <EmptyState
                    icon="key"
                    title="Create a gateway key first"
                    description="Playground uses an existing key from API keys. The relay uses it server-side; the key secret is never sent to the browser."
                    action={
                      <Link className="btn btn-primary" to="/tokens">
                        <Icon name="key" size={15} /> Go to API keys
                      </Link>
                    }
                  />
                ) : !keyUsable ? (
                  <EmptyState
                    icon="alert"
                    title={keyExpired ? 'This key has expired' : 'This key is disabled'}
                    description="Select an active gateway key in the panel on the right, or update this key on the API keys page."
                  />
                ) : modelsLoading ? (
                  <div className="page-loading">
                    <Spinner size={18} /> Loading models for this key…
                  </div>
                ) : !modelReady ? (
                  <EmptyState
                    icon={modelsError ? 'alert' : 'models'}
                    title={modelsError ? 'Could not load models' : 'No models available for this key'}
                    description={emptyModelMessage}
                    action={
                      <button className="btn btn-ghost" onClick={() => setModelsRefresh((value) => value + 1)}>
                        <Icon name="refresh" size={15} /> Retry
                      </button>
                    }
                  />
                ) : (
                  <EmptyState
                    icon="spark"
                    title="Start a real request"
                    description="Pick a model and send a message. Responses stream in, keep multi-turn context, can be stopped, and can be traced in Logs by request ID."
                  />
                )}
              </div>
            ) : (
              messages.map((message, index) => {
                const isLast = index === messages.length - 1
                const roleLabel = message.role === 'user' ? 'You' : message.request?.model || 'Assistant'
                return (
                  <article className={`chat-message ${message.role === 'user' ? 'from-user' : 'from-assistant'}`} key={message.id}>
                    <div className="chat-avatar">{message.role === 'user' ? 'You' : 'AI'}</div>
                    <div className="chat-body">
                      <div className="chat-role">{roleLabel}</div>
                      {message.content ? (
                        <div className="chat-content">
                          {message.content}
                          {message.status === 'streaming' ? <span className="chat-caret" aria-hidden="true" /> : null}
                        </div>
                      ) : message.status === 'streaming' ? (
                        <div className="chat-thinking">
                          <Spinner size={13} /> Waiting for the first chunk…
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
                          Generation stopped. Content already received may still be billed.
                        </div>
                      ) : null}

                      {message.role === 'assistant' && message.status !== 'streaming' ? (
                        <div className="chat-meta">
                          {message.request ? (
                            <span>
                              key {message.request.keyName} · {message.request.model} · temp{' '}
                              {message.request.temperature.toFixed(1)} · max output{' '}
                              {message.request.maxTokens ?? 'default'}
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
                                  ? 'The upstream did not report usage; the relay estimated it from the request and returned text.'
                                  : 'Token usage reported by the upstream.'
                              }
                            >
                              Tokens {formatNumber(message.usage.total)} ({formatNumber(message.usage.prompt)} in /{' '}
                              {formatNumber(message.usage.completion)} out){message.usage.source === 'estimated' ? ' · estimated' : ''}
                            </span>
                          ) : (
                            <span className="muted">Token usage unavailable for this response</span>
                          )}
                          {message.requestId ? (
                            <span className="request-id">
                              Request <code>{message.requestId}</code>
                              <button
                                className="link-button"
                                type="button"
                                onClick={() => void copyRequestId(message.requestId ?? '')}
                              >
                                Copy
                              </button>
                              <Link className="link-button" to={`/logs?request_id=${encodeURIComponent(message.requestId)}`}>
                                View log
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
                                ? 'Fix the max output value first'
                                : 'Creates a new gateway request and may incur new charges'
                            }
                          >
                            <Icon name="refresh" size={14} /> Regenerate
                          </button>
                          <span className="chat-action-hint">Sends a new request and may incur new charges.</span>
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
                  ? 'Type a message. Enter to send, Shift+Enter for a new line.'
                  : 'Select a gateway key and model above.'
              }
              disabled={!canType}
              aria-label="Message"
            />
            <div className="composer-bar">
              <span className="composer-hint">
                {maxTokensInvalid
                  ? `Max output must be an integer between 1 and ${maxOutputTokens}.`
                  : 'Requests go through this relay; this page never calls an upstream directly.'}
              </span>
              {sending ? (
                <button className="btn btn-danger" onClick={stopGeneration}>
                  <Icon name="stop" size={14} /> Stop generating
                </button>
              ) : (
                <button className="btn btn-primary" onClick={sendMessage} disabled={!canSend}>
                  <Icon name="send" size={15} /> Send
                </button>
              )}
            </div>
            <p className="composer-note">
              Every send or regeneration creates a new gateway request and may incur charges. Provider billing
              applies; the relay's cost display depends on configured channel prices. This page never retries
              automatically.
            </p>
          </div>
        </section>

        <aside className="playground-side">
          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>Gateway key</h2>
              <div className="key-head-actions">
                {keyBadge}
                <button
                  className="icon-button"
                  type="button"
                  onClick={() => void loadKeys()}
                  disabled={keysLoading || sending}
                  title="Refresh API keys"
                  aria-label="Refresh gateway keys"
                >
                  <Icon name="refresh" size={16} />
                </button>
              </div>
            </div>
            <label className="field">
              <span className="field-label">Use an existing key</span>
              <select
                className="input"
                value={selectedKeyId ?? ''}
                onChange={(event) => setSelectedKeyId(event.target.value ? Number(event.target.value) : null)}
                disabled={keysLoading || sending || keys.length === 0}
                aria-label="Gateway key"
              >
                {keys.length === 0 ? (
                  <option value="">{keysLoading ? 'Loading keys…' : 'No gateway keys'}</option>
                ) : (
                  keys.map((key) => (
                    <option value={key.id} key={key.id}>
                      {key.name} · {key.key_prefix} · {key.status}
                    </option>
                  ))
                )}
              </select>
              <span className="field-hint">
                The relay uses this key server-side; the browser never sees its secret. Model access, quota, RPM limit
                and request logs apply.
              </span>
            </label>
            {selectedKey ? (
              <div className="key-meta">
                <div>
                  <span>Quota</span>
                  <strong>
                    {selectedKey.quota < 0
                      ? 'Unlimited'
                      : `${formatCost(selectedKey.used_quota)} / ${formatCost(selectedKey.quota)}`}
                  </strong>
                </div>
                <div>
                  <span>Rate limit</span>
                  <strong>{selectedKey.rpm_limit > 0 ? `${selectedKey.rpm_limit} / min` : 'Unlimited'}</strong>
                </div>
                <div>
                  <span>Model access</span>
                  <strong>
                    {selectedKey.allowed_models.length > 0
                      ? `${selectedKey.allowed_models.length} allowed`
                      : 'All models'}
                  </strong>
                </div>
                <div>
                  <span>Expires</span>
                  <strong>{selectedKey.expires_at ? formatTime(selectedKey.expires_at) : 'Never'}</strong>
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
              Manage keys in <Link to="/tokens">API keys</Link>.
            </p>
          </section>

          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>Model</h2>
              {modelsLoading ? (
                <Spinner size={13} />
              ) : modelReady ? (
                <span className="badge badge-muted">{models.length} available</span>
              ) : null}
            </div>
            <select
              className="input"
              value={selectedModel}
              onChange={(event) => setSelectedModel(event.target.value)}
              disabled={!keyUsable || modelsLoading || sending}
              aria-label="Model"
            >
              {modelsLoading ? (
                <option value="">Loading models…</option>
              ) : models.length === 0 ? (
                <option value="">{keyUsable ? 'No models available' : 'Select an active gateway key'}</option>
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
                    <>Cost price is not configured for this channel. Provider billing still applies.</>
                  ) : (
                    <>
                      Input {formatPrice(selectedInfo.input_price)} / 1M · Output{' '}
                      {formatPrice(selectedInfo.output_price)} / 1M{' '}
                      <span className="muted">(configured prices used for cost estimates)</span>
                    </>
                  )}
                </div>
              </div>
            ) : null}
            {catalogError ? <div className="field-hint">Failed to load the model catalog: {catalogError}</div> : null}
            <p className="field-hint model-note">
              Available models come from the key's model allowlist and enabled channels. Switching models only affects
              the next request; the conversation is not resent automatically, and history stays in context.
            </p>
          </section>

          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>Chat parameters</h2>
            </div>
            <label className="field">
              <span className="field-label">System prompt</span>
              <textarea
                className="input textarea"
                rows={3}
                value={systemPrompt}
                onChange={(event) => setSystemPrompt(event.target.value)}
                placeholder="Optional. For example: You are a concise technical assistant."
                disabled={sending}
                aria-label="System prompt"
              />
              <span className="field-hint">Changes apply from the next request.</span>
            </label>

            <div className="field">
              <div className="param-head">
                <span className="field-label">Temperature</span>
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
                aria-label="Temperature"
              />
              <span className="field-hint">{temperatureHint}</span>
            </div>

            <label className="field">
              <span className="field-label">Max output tokens</span>
              <input
                className={`input${maxTokensInvalid ? ' input-invalid' : ''}`}
                type="number"
                min={1}
                max={maxOutputTokens}
                step={1}
                value={maxTokens}
                onChange={(event) => setMaxTokens(event.target.value)}
                placeholder="Leave empty for the default"
                disabled={sending}
                aria-label="Maximum output tokens"
              />
              <span className="field-hint">
                1–{maxOutputTokens}, or empty for the model/relay default. The exact model limit is enforced by the
                upstream; if exceeded, its error is shown here.
              </span>
            </label>

            <div className="param-info">
              <Icon name="shield" size={14} />
              <span>
                Parameter ranges follow the selected model's channel protocol. If the model rejects a parameter, the
                upstream error is shown in the conversation.
              </span>
            </div>
          </section>
        </aside>
      </div>

      <Modal
        open={confirm?.kind === 'retry'}
        onClose={() => setConfirm(null)}
        title="Send this turn again?"
        subtitle="This creates a new gateway request and may incur new charges."
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setConfirm(null)}>
              Cancel
            </button>
            <button className="btn btn-primary" onClick={confirmRetry} disabled={!canRetry}>
              Send new request
            </button>
          </>
        }
      >
        <p className="modal-copy">
          The last user message will be sent again and the current assistant reply will be replaced. The request uses
          the page's current settings:
        </p>
        <div className="confirm-params">
          <div>
            <span>Model</span>
            <strong className="mono">{selectedModel || '—'}</strong>
          </div>
          <div>
            <span>Temperature</span>
            <strong>{temperature.toFixed(1)}</strong>
          </div>
          <div>
            <span>Max output</span>
            <strong>{maxTokensValue ?? 'Default'}</strong>
          </div>
        </div>
        {retryMessage?.status === 'stopped' ? (
          <p className="modal-copy">The previous generation was stopped; the new request starts from scratch.</p>
        ) : null}
        <p className="modal-copy">
          Nothing is retried automatically. A request is sent only after you click “Send new request”.
        </p>
      </Modal>

      <Modal
        open={confirm?.kind === 'clear'}
        onClose={() => setConfirm(null)}
        title="Clear this conversation?"
        subtitle="The system prompt and parameters are kept."
        footer={
          <>
            <button className="btn btn-ghost" onClick={() => setConfirm(null)}>
              Cancel
            </button>
            <button className="btn btn-danger" onClick={confirmClear}>
              Clear conversation
            </button>
          </>
        }
      >
        <p className="modal-copy">
          All messages on this page will be removed. Existing request logs and charges remain available in Logs.
        </p>
      </Modal>
    </div>
  )
}
