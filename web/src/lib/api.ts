import type {
  ApiStatus,
  Channel,
  ChannelInput,
  Dashboard,
  LedgerResponse,
  LogsResponse,
  LogQuery,
  ModelInfo,
  SpeechClip,
  SpeechClipsResponse,
  SpeechInput,
  SpeechVoiceCatalog,
  TestResult,
  Token,
  TokenInput,
  UpstreamModelsInput,
  User,
  VideoInput,
  VideosResponse,
  VideoTask,
  VoiceClone,
  VoiceCloneInput,
  VoiceClonesResponse,
} from './types'

export class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: 'same-origin',
  })
  const text = await response.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!response.ok) {
    if (response.status === 401 && !path.startsWith('/api/auth/login')) {
      window.dispatchEvent(new Event('mr:unauthorized'))
    }
    let message = `Request failed with status ${response.status}`
    if (data && typeof data === 'object' && 'error' in data) {
      const error = (data as { error: unknown }).error
      if (typeof error === 'string') {
        message = error
      } else if (error && typeof error === 'object' && 'message' in error) {
        message = String((error as { message: unknown }).message)
      }
    }
    throw new ApiError(message, response.status)
  }
  return data as T
}

function jsonBody(value: unknown): RequestInit {
  return { body: JSON.stringify(value) }
}

export const api = {
  login(username: string, password: string) {
    return request<{ user: User }>('/api/auth/login', {
      method: 'POST',
      ...jsonBody({ username, password }),
    })
  },
  logout() {
    return request<{ ok: boolean }>('/api/auth/logout', { method: 'POST' })
  },
  me() {
    return request<{ user: User }>('/api/auth/me')
  },
  status() {
    return request<ApiStatus>('/api/status')
  },
  dashboard() {
    return request<Dashboard>('/api/dashboard')
  },
  channels() {
    return request<{ data: Channel[] }>('/api/channels')
  },
  createChannel(input: ChannelInput) {
    return request<Channel>('/api/channels', { method: 'POST', ...jsonBody(input) })
  },
  updateChannel(id: number, input: ChannelInput) {
    return request<Channel>(`/api/channels/${id}`, { method: 'PUT', ...jsonBody(input) })
  },
  deleteChannel(id: number) {
    return request<null>(`/api/channels/${id}`, { method: 'DELETE' })
  },
  testChannel(id: number, model?: string) {
    const query = model ? `?model=${encodeURIComponent(model)}` : ''
    return request<TestResult>(`/api/channels/${id}/test${query}`, { method: 'POST' })
  },
  fetchUpstreamModels(input: UpstreamModelsInput) {
    return request<{ models: string[] }>('/api/channels/upstream-models', { method: 'POST', ...jsonBody(input) })
  },
  tokens() {
    return request<{ data: Token[] }>('/api/tokens')
  },
  createToken(input: TokenInput) {
    return request<{ token: Token; key: string }>('/api/tokens', { method: 'POST', ...jsonBody(input) })
  },
  updateToken(id: number, input: TokenInput) {
    return request<Token>(`/api/tokens/${id}`, { method: 'PUT', ...jsonBody(input) })
  },
  deleteToken(id: number) {
    return request<null>(`/api/tokens/${id}`, { method: 'DELETE' })
  },
  logs(query: LogQuery) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null || value === '') continue
      params.set(key, String(value))
    }
    const suffix = params.toString()
    return request<LogsResponse>(`/api/logs${suffix ? `?${suffix}` : ''}`)
  },
  models() {
    return request<{ data: ModelInfo[] }>('/api/models')
  },
  ledger(limit = 100) {
    return request<LedgerResponse>(`/api/ledger?limit=${limit}`)
  },
  videos() {
    return request<VideosResponse>('/api/videos?page_size=50')
  },
  createVideo(input: VideoInput) {
    return request<VideoTask>('/api/videos', { method: 'POST', ...jsonBody(input) })
  },
  speech() {
    return request<SpeechClipsResponse>('/api/speech?page_size=50')
  },
  speechVoices(model?: string) {
    const query = model ? `?model=${encodeURIComponent(model)}` : ''
    return request<SpeechVoiceCatalog>(`/api/speech/voices${query}`)
  },
  createSpeech(input: SpeechInput) {
    return request<SpeechClip>('/api/speech', { method: 'POST', ...jsonBody(input) })
  },
  voiceClones() {
    return request<VoiceClonesResponse>('/api/voice-clones')
  },
  createVoiceClone(input: VoiceCloneInput) {
    const form = new FormData()
    form.set('name', input.name)
    form.set('model', input.model)
    form.set('preview_text', input.preview_text)
    form.set('authorized', input.authorized ? 'true' : 'false')
    form.set('file', input.file)
    return request<VoiceClone>('/api/voice-clones', { method: 'POST', body: form })
  },
  previewVoiceClone(id: number, text: string) {
    return request<VoiceClone>(`/api/voice-clones/${id}/preview`, { method: 'POST', ...jsonBody({ text }) })
  },
  playgroundModels(tokenId: number) {
    return request<{ data: { id: string }[] }>(`/api/playground/models?token_id=${tokenId}`)
  },
}
