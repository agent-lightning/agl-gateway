// A time range expressed as backend query params. Presets send a Go-duration `since`
// (e.g. "24h"); a custom range sends RFC3339 `since`/`until` bounds. "All time" sends
// neither. The admin endpoints accept RFC3339, a Go duration, or unix millis for both.

export interface TimeQuery {
  since?: string
  until?: string
}

export interface TimePreset {
  value: string
  label: string
  query: TimeQuery
}

export const TIME_PRESETS: TimePreset[] = [
  { value: 'all', label: 'All time', query: {} },
  { value: '5m', label: 'Last 5 minutes', query: { since: '5m' } },
  { value: '15m', label: 'Last 15 minutes', query: { since: '15m' } },
  { value: '1h', label: 'Last hour', query: { since: '1h' } },
  { value: '6h', label: 'Last 6 hours', query: { since: '6h' } },
  { value: '24h', label: 'Last 24 hours', query: { since: '24h' } },
  { value: '168h', label: 'Last 7 days', query: { since: '168h' } },
  { value: '720h', label: 'Last 30 days', query: { since: '720h' } },
]

export const DEFAULT_PRESET = '24h'

export function presetQuery(value: string): TimeQuery {
  return TIME_PRESETS.find((p) => p.value === value)?.query ?? {}
}

/** Append since/until query params to a URLSearchParams when present. */
export function applyTimeQuery(params: URLSearchParams, q: TimeQuery) {
  if (q.since) params.set('since', q.since)
  if (q.until) params.set('until', q.until)
}

/** Convert a <input type="datetime-local"> value to an RFC3339 string in the local zone. */
export function localInputToRFC3339(v: string): string | undefined {
  if (!v) return undefined
  const d = new Date(v)
  if (isNaN(d.getTime())) return undefined
  return d.toISOString()
}
