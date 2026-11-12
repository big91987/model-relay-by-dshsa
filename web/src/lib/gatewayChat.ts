// Client helpers for the Playground's admin-authenticated relay endpoint. The
// browser sends only the selected gateway-key id; the key secret stays on the
// server and is never persisted by this module.

export interface GatewayError {
  message: string
  type?: string
  code?: string
}

export interface ChatUsage {
  prompt: number
  completion: number
  total: number
  source: 'upstream' | 'estimated'
}

export interface ChatStreamResult {
  content: string
  usage?: ChatUsage
  requestId?: string
  aborted: boolean
  error?: string
}

export class GatewayRequestError extends Error {
  status: number
  type?: string
  code?: string
  requestId?: string

  constructor(error: GatewayError, status: number, requestId?: string) {
    super(describeGatewayError(status, error))
    this.name = 'GatewayRequestError'
    this.status = status
    this.type = error.type
    this.code = error.code
    this.requestId = requestId
  }
}

export async function readGatewayError(response: Response): Promise<GatewayError> {
  try {
    const data = (await response.json()) as { error?: unknown }
    const error = data?.error
    if (typeof error === 'string') {
      return { message: error }
    }
    if (error && typeof error === 'object') {
      const fields = error as Record<string, unknown>
      return {
        message: typeof fields.message === 'string' ? fields.message : `HTTP ${response.status}`,
        type: typeof fields.type === 'string' ? fields.type : undefined,
        code: typeof fields.code === 'string' ? fields.code : undefined,
      }
    }
  } catch {
    // Fall through to the generic HTTP message.
  }
  return { message: `HTTP ${response.status}` }
}

export function describeGatewayError(status: number, error: GatewayError): string {
  const message = error.message?.trim() || `Request failed with HTTP ${status}`
  if (status === 401 && !error.code && /authentication|session/i.test(message)) {
    return 'Your admin session expired. Sign in again.'
  }
  if (status === 401) return `Invalid or disabled gateway key: ${message}`
  if (status === 403) return `This key is not allowed to use the selected model: ${message}`
  if (error.type === 'insufficient_quota' || /insufficient quota/i.test(message)) {
    return `Insufficient quota: ${message}`
  }
  if (error.type === 'rate_limit_exceeded' || (status === 429 && error.code !== 'upstream_unavailable')) {
    return `Rate limited: ${message}`
  }
  if (error.code === 'model_not_found') return `Model unavailable: ${message}`
  if (error.code === 'upstream_unavailable') {
    if (/rate limit/i.test(message)) {
      return `Upstream channel is rate limited: ${message}. Open the request log for channel details.`
    }
    return `Upstream unavailable: ${message}. Check the serving channel and open the request log for details.`
  }
  if (status === 429) return `Rate limited or out of quota: ${message}`
  if (status >= 500) return `Upstream or relay error: ${message}`
  return message
}

// streamChatCompletion calls the admin Playground chat endpoint with
// stream=true and incrementally reports the assistant text. The selected key id
// authorizes the relay request; its secret never reaches the browser. It
// returns partial content even when the user aborts or the connection breaks,
// so the page can keep showing what was actually received.
export async function streamChatCompletion(options: {
  tokenId: number
  body: Record<string, unknown>
  signal: AbortSignal
  onDelta: (content: string) => void
}): Promise<ChatStreamResult> {
  const { tokenId, body, signal, onDelta } = options
  const result: ChatStreamResult = { content: '', aborted: false }

  let response: Response
  try {
    response = await fetch(`/api/playground/chat?token_id=${encodeURIComponent(String(tokenId))}`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
      },
      body: JSON.stringify(body),
      signal,
    })
  } catch (error) {
    if (signal.aborted) {
      result.aborted = true
      return result
    }
    throw new Error(`Cannot reach the relay: ${error instanceof Error ? error.message : String(error)}`)
  }

  result.requestId = response.headers.get('X-Request-Id') ?? undefined
  if (!response.ok) {
    const error = await readGatewayError(response)
    if (response.status === 401 && !error.code && /authentication|session/i.test(error.message)) {
      window.dispatchEvent(new Event('mr:unauthorized'))
    }
    throw new GatewayRequestError(error, response.status, result.requestId)
  }
  if (!response.body) {
    throw new Error('The browser cannot read a streaming response.')
  }

  const contentType = response.headers.get('Content-Type') ?? ''
  if (contentType.includes('application/json')) {
    const data = (await response.json()) as {
      choices?: { message?: { content?: unknown } }[]
      usage?: Record<string, unknown>
    }
    const content = data.choices?.[0]?.message?.content
    if (typeof content === 'string' && content) {
      result.content = content
      onDelta(content)
    }
    result.usage = parseUsage(data.usage, undefined)
    return result
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let finished = false

  const consume = (block: string): boolean => {
    const payload = block
      .split('\n')
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trimStart())
      .join('\n')
      .trim()
    if (!payload) return false
    if (payload === '[DONE]') return true
    let event: Record<string, unknown>
    try {
      event = JSON.parse(payload) as Record<string, unknown>
    } catch {
      return false
    }
    if (event.error && typeof event.error === 'object') {
      const fields = event.error as Record<string, unknown>
      const message = typeof fields.message === 'string' ? fields.message : 'The upstream returned an error'
      result.error =
        fields.code === 'stream_interrupted'
          ? `Connection interrupted: the upstream stream ended before the response finished. Partial content is shown above. (${message})`
          : message
    }
    const delta = (event.choices as { delta?: { content?: unknown } }[] | undefined)?.[0]?.delta?.content
    if (typeof delta === 'string' && delta) {
      result.content += delta
      onDelta(result.content)
    }
    if (event.usage && typeof event.usage === 'object') {
      result.usage = parseUsage(event.usage as Record<string, unknown>, event.x_usage_source)
    }
    return false
  }

  try {
    while (!finished) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      buffer = buffer.replace(/\r\n/g, '\n')
      let boundary = buffer.indexOf('\n\n')
      while (boundary >= 0) {
        const block = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + 2)
        if (consume(block)) {
          finished = true
          break
        }
        boundary = buffer.indexOf('\n\n')
      }
    }
    if (!finished) {
      buffer += decoder.decode()
      buffer = buffer.replace(/\r\n/g, '\n')
      if (buffer.trim()) consume(buffer)
    }
  } catch (error) {
    if (signal.aborted) {
      result.aborted = true
      return result
    }
    result.error = `Connection interrupted: ${error instanceof Error ? error.message : String(error)}`
  }

  if (!finished && !result.error && !signal.aborted) {
    if (!result.usage) {
      result.error = 'Connection interrupted: the response ended before completion.'
    }
  }

  return result
}

function parseUsage(usage: Record<string, unknown> | undefined, source: unknown): ChatUsage | undefined {
  if (!usage) return undefined
  const prompt = numberValue(usage.prompt_tokens)
  const completion = numberValue(usage.completion_tokens)
  const total = numberValue(usage.total_tokens) || prompt + completion
  if (prompt === 0 && completion === 0 && total === 0) return undefined
  return {
    prompt,
    completion,
    total,
    source: source === 'estimated' ? 'estimated' : 'upstream',
  }
}

function numberValue(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : 0
}
