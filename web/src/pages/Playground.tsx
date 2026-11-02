import { useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { Link } from 'react-router-dom'
import { Icon } from '../components/Icon'
import { Modal } from '../components/Modal'
import { EmptyState, PageHeader, Spinner } from '../components/ui'
import { api } from '../lib/api'
import {
  describeGatewayError,
  GatewayRequestError,
  readGatewayError,
  streamChatCompletion,
} from '../lib/gatewayChat'
import type { ChatUsage } from '../lib/gatewayChat'
import { formatDuration, formatNumber, formatPrice } from '../lib/format'
import { useToast } from '../lib/toast'
import type { ModelInfo } from '../lib/types'

type MessageRole = 'user' | 'assistant'
type MessageStatus = 'streaming' | 'complete' | 'stopped' | 'error'
type KeyStatus = 'idle' | 'checking' | 'ready' | 'empty' | 'error'

interface RequestSnapshot {
  model: string
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

interface KeyState {
  status: KeyStatus
  message?: string
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
  const [gatewayKey, setGatewayKey] = useState('')
  const [showKey, setShowKey] = useState(false)
  const [keyState, setKeyState] = useState<KeyState>({ status: 'idle' })
  const [models, setModels] = useState<string[]>([])
  const [selectedModel, setSelectedModel] = useState('')
  const [systemPrompt, setSystemPrompt] = useState('')
  const [temperature, setTemperature] = useState(defaultTemperature)
  const [maxTokens, setMaxTokens] = useState('')
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState('')
  const [sending, setSending] = useState(false)
  const [confirm, setConfirm] = useState<ConfirmState>(null)
  const abortRef = useRef<AbortController | null>(null)
  const keyInputRef = useRef<HTMLInputElement>(null)
  const scrollRef = useRef<HTMLDivElement>(null)

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

  const modelReady = keyState.status === 'ready' && models.length > 0 && selectedModel !== ''
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
    if (catalogError) {
      return 'The relay reported no available models for this key. Check the key allowlist and enabled channels.'
    }
    if (catalog.length === 0) {
      return 'The gateway has no available models. Add and enable a channel in Channels first.'
    }
    return 'This key has no available models: its model allowlist may not match any enabled channel, or the channels are disabled.'
  }, [catalog.length, catalogError])

  const validateKey = async () => {
    const key = gatewayKey.trim()
    if (!key) {
      setKeyState({ status: 'error', message: 'Enter a gateway key first.' })
      return
    }
    setKeyState({ status: 'checking' })
    try {
      const response = await fetch('/v1/models', {
        method: 'GET',
        credentials: 'same-origin',
        headers: { Authorization: `Bearer ${key}` },
      })
      if (!response.ok) {
        const error = await readGatewayError(response)
        setKeyState({ status: 'error', message: describeGatewayError(response.status, error) })
        setModels([])
        setSelectedModel('')
        return
      }
      const data = (await response.json()) as { data?: { id?: string }[] }
      const available = (data.data ?? [])
        .map((model) => model.id)
        .filter((id): id is string => typeof id === 'string' && id !== '')
      setModels(available)
      if (available.length === 0) {
        setKeyState({ status: 'empty', message: emptyModelMessage })
        setSelectedModel('')
        return
      }
      setSelectedModel((current) => (available.includes(current) ? current : available[0]))
      setKeyState({ status: 'ready' })
      toast.success(`Gateway key is valid. ${available.length} model${available.length === 1 ? '' : 's'} available.`)
    } catch (error) {
      setKeyState({
        status: 'error',
        message: `Cannot reach the relay: ${error instanceof Error ? error.message : String(error)}`,
      })
    }
  }

  const patchAssistant = (id: string, patch: Partial<ChatMessage>) => {
    setMessages((current) => current.map((message) => (message.id === id ? { ...message, ...patch } : message)))
  }

  const runCompletion = async (history: ChatMessage[]) => {
    const key = gatewayKey.trim()
    if (!key) {
      setKeyState({ status: 'error', message: 'Enter a gateway key first.' })
      return
    }
    if (!selectedModel) return

    const request: RequestSnapshot = {
      model: selectedModel,
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
        apiKey: key,
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

  const keyBadge = (() => {
    switch (keyState.status) {
      case 'ready':
        return <span className="badge badge-success"><span className="badge-dot badge-dot-success" />Connected</span>
      case 'checking':
        return <span className="badge badge-muted"><span className="badge-dot badge-dot-muted" />Checking</span>
      case 'empty':
        return <span className="badge badge-danger"><span className="badge-dot badge-dot-danger" />No models</span>
      case 'error':
        return <span className="badge badge-danger"><span className="badge-dot badge-dot-danger" />Failed</span>
      default:
        return <span className="badge badge-muted"><span className="badge-dot badge-dot-muted" />Not connected</span>
    }
  })()

  return (
    <div className="page playground-page">
      <PageHeader
        title="Playground"
        subtitle="Try chat models with an admin session and a gateway key. Requests go through the real relay and inherit the key's model access, quota, rate limit and request logs."
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
                {!modelReady ? (
                  <EmptyState
                    icon="key"
                    title={keyState.status === 'empty' ? 'This key has no available models' : 'Connect a gateway key first'}
                    description={
                      keyState.status === 'empty'
                        ? emptyModelMessage
                        : 'Enter and validate a key to choose a model. The key stays in this page memory only.'
                    }
                    action={
                      <button className="btn btn-primary" onClick={() => keyInputRef.current?.focus()}>
                        <Icon name="key" size={15} /> Enter a gateway key
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
                              {message.request.model} · temp {message.request.temperature.toFixed(1)} · max output{' '}
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
                  : 'Validate a gateway key and choose a model above.'
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
              Every send or regeneration creates a new gateway request and may incur charges. This page never retries
              automatically.
            </p>
          </div>
        </section>

        <aside className="playground-side">
          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>Gateway connection</h2>
              {keyBadge}
            </div>
            <label className="field">
              <span className="field-label">Gateway key</span>
              <div className="input-row">
                <input
                  ref={keyInputRef}
                  className="input"
                  type={showKey ? 'text' : 'password'}
                  value={gatewayKey}
                  onChange={(event) => {
                    setGatewayKey(event.target.value)
                    if (keyState.status !== 'idle') {
                      setKeyState({ status: 'idle' })
                      setModels([])
                      setSelectedModel('')
                    }
                  }}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') {
                      event.preventDefault()
                      void validateKey()
                    }
                  }}
                  placeholder="sk-…"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={sending}
                  aria-label="Gateway key"
                />
                <button
                  className="icon-button"
                  type="button"
                  onClick={() => setShowKey((current) => !current)}
                  title={showKey ? 'Hide key' : 'Show key'}
                  aria-label={showKey ? 'Hide gateway key' : 'Show gateway key'}
                  disabled={sending}
                >
                  <Icon name={showKey ? 'eyeOff' : 'eye'} size={17} />
                </button>
              </div>
              <span className="field-hint">
                The key is held in this page memory only. Refreshing or leaving the page clears it; it is not written to
                the URL, browser storage or application logs.
              </span>
            </label>
            <button
              className="btn btn-ghost btn-block side-action"
              onClick={() => void validateKey()}
              disabled={keyState.status === 'checking' || !gatewayKey.trim() || sending}
            >
              {keyState.status === 'checking' ? <Spinner size={14} /> : <Icon name="test" size={15} />}
              Validate key &amp; load models
            </button>
            {keyState.status === 'error' && keyState.message ? (
              <div className="side-error" role="alert">
                <Icon name="alert" size={14} />
                <span>{keyState.message}</span>
              </div>
            ) : null}
            {keyState.status === 'empty' && keyState.message ? (
              <div className="side-error" role="alert">
                <Icon name="alert" size={14} />
                <span>
                  {keyState.message}{' '}
                  {catalog.length === 0 ? <Link to="/channels">Add a channel</Link> : <Link to="/tokens">Check key access</Link>}
                </span>
              </div>
            ) : null}
            <p className="side-note">
              Need a gateway key? <Link to="/tokens">Create one in API keys</Link>
            </p>
          </section>

          <section className="panel side-panel">
            <div className="side-panel-head">
              <h2>Model</h2>
              {keyState.status === 'ready' ? <span className="badge badge-muted">{models.length} available</span> : null}
            </div>
            <select
              className="input"
              value={selectedModel}
              onChange={(event) => setSelectedModel(event.target.value)}
              disabled={keyState.status !== 'ready' || sending}
              aria-label="Model"
            >
              {models.length === 0 ? (
                <option value="">{keyState.status === 'ready' ? 'No models available' : 'Validate a key first'}</option>
              ) : (
                models.map((model) => (
                  <option value={model} key={model}>
                    {model}
                  </option>
                ))
              )}
            </select>
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
                  Input {formatPrice(selectedInfo.input_price)} / 1M · Output {formatPrice(selectedInfo.output_price)} / 1M{' '}
                  <span className="muted">(configured prices used for cost estimates)</span>
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
