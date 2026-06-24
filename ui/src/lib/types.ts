// Types mirror the Go JSON shapes in internal/store and internal/admin. Go []byte fields
// (raw_request/raw_response/assembled_response) are base64-encoded strings over the wire.

export interface APIKey {
  id: number
  name: string
  prefix: string
  providers: string[]
  provider_start: string
  provider_order: string
  created_at: string
  disabled: boolean
  keep_logs_on_delete: boolean
}

export interface CreatedKey extends APIKey {
  key: string // plaintext, returned only once
}

export interface RequestLog {
  // Serialized by the gateway as a string: ClickHouse log ids exceed JS's safe-integer range
  // and would round if parsed as a number.
  id: string
  api_key_id: number
  key_name: string
  provider: string
  model: string
  mapped_model: string
  method: string
  path: string
  query: string
  client_addr: string
  user_agent: string
  request_content_type: string
  response_content_type: string
  request_bytes: number
  response_bytes: number
  status_code: number
  streaming: boolean
  api_type?: string
  attempts: number
  ttft_ms: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
  error: string
  assemble_error?: string
  raw_request?: string
  raw_response?: string
  assembled_response?: string
  raw_request_truncated: boolean
  raw_response_truncated: boolean
  assembled_response_truncated: boolean
  created_at: string
}

export interface LogsResponse {
  logs: RequestLog[]
  limit: number
  offset: number
  next_offset?: number
  has_more: boolean
  api_key?: APIKey
}

export interface Stat {
  api_key_id: number
  key_name: string
  model: string
  requests: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cost: number
}

export interface ProviderInfo {
  name: string
  models: string[]
  error?: string
}

export interface ProbeResult {
  provider: string
  model: string
  endpoint: string
  status: number
  attempts?: string
  duration_ms: number
  detail: string
  ok: boolean
}

export interface TestEvent {
  type: 'start' | 'result' | 'done' | 'error'
  total?: number
  passed?: number
  failed?: number
  skipped?: number
  result?: ProbeResult
  message?: string
}

export type ProviderStart = 'first' | 'random'
export type ProviderOrder = 'round_robin' | 'random'
