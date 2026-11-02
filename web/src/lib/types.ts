export interface User {
  id: number
  username: string
  role: string
  status: string
  created_at: number
}

export type ChannelType = 'openai' | 'anthropic'
export type ChannelStatus = 'enabled' | 'disabled'

export interface Channel {
  id: number
  name: string
  type: ChannelType
  base_url: string
  api_key_hint: string
  models: string[]
  model_mapping: Record<string, string>
  priority: number
  weight: number
  status: ChannelStatus
  input_price: number
  output_price: number
  created_at: number
  updated_at: number
}

export interface ChannelInput {
  name: string
  type: ChannelType
  base_url: string
  api_key?: string
  models: string[]
  model_mapping: Record<string, string>
  priority: number
  weight: number
  status: ChannelStatus
  input_price: number
  output_price: number
}

export interface TestResult {
  ok: boolean
  latency_ms: number
  status_code: number
  message: string
}

export type TokenStatus = 'active' | 'disabled'

export interface Token {
  id: number
  user_id: number
  name: string
  key_prefix: string
  status: TokenStatus
  quota: number
  used_quota: number
  reserved_quota: number
  allowed_models: string[]
  rpm_limit: number
  expires_at: number | null
  last_used_at: number | null
  created_at: number
}

export interface TokenInput {
  name: string
  quota: number
  allowed_models: string[]
  rpm_limit: number
  expires_at: number | null
  status: TokenStatus
}

export type CostStatus = 'actual' | 'estimated' | 'unverified' | 'free' | 'pending'

export interface LogItem {
  id: number
  request_id: string
  reservation_id: string
  user_id: number
  token_id: number
  channel_id: number
  token_name: string
  channel_name: string
  model: string
  upstream_model: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
  reserved_cost: number
  cost_status: CostStatus | string
  status: 'success' | 'error' | string
  status_code: number
  error: string
  latency_ms: number
  stream: boolean
  created_at: number
}

export interface LedgerItem {
  id: string
  token_id: number
  token_name: string
  request_id: string
  status: 'pending' | 'settled' | 'released' | 'recovered' | string
  reserved_cost: number
  settled_cost: number
  cost_status: CostStatus | string
  prompt_tokens: number
  completion_tokens: number
  model: string
  upstream_model: string
  channel_id: number
  channel_name: string
  stream: boolean
  created_at: number
  settled_at: number | null
  expires_at: number
}

export interface LedgerResponse {
  data: LedgerItem[]
  pending_cost: number
}

export interface LogsResponse {
  items: LogItem[]
  total: number
  page: number
  page_size: number
}

export interface Totals {
  requests: number
  success: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost: number
  unverified_cost: number
}

export interface DayStat {
  date: string
  requests: number
  tokens: number
  cost: number
  errors: number
}

export interface ModelStat {
  model: string
  requests: number
  tokens: number
}

export interface CountPair {
  total: number
  active: number
}

export interface Dashboard {
  totals: Totals
  today: Totals
  series: DayStat[]
  top_models: ModelStat[]
  recent_logs: LogItem[]
  channels: CountPair
  tokens: CountPair
}

export interface ModelInfo {
  id: string
  channels: string[]
  types: string[]
  input_price: number
  output_price: number
}

export interface LogQuery {
  page?: number
  page_size?: number
  model?: string
  status?: string
  token_id?: number
  start?: number
  end?: number
}

export interface ApiStatus {
  name: string
  version: string
  uptime_seconds: number
}
