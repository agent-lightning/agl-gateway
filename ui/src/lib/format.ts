// Display formatters and payload decoding helpers.

/** Decode a base64 string (Go []byte JSON encoding) into a UTF-8 string. */
export function decodeBase64(b64: string): string {
  try {
    const bin = atob(b64)
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
    return new TextDecoder().decode(bytes)
  } catch {
    return ''
  }
}

/** Byte length of a base64 payload, without fully decoding it. */
export function base64ByteLength(b64: string): number {
  if (!b64) return 0
  let padding = 0
  if (b64.endsWith('==')) padding = 2
  else if (b64.endsWith('=')) padding = 1
  return Math.floor((b64.length * 3) / 4) - padding
}

/** Pretty-print a JSON string; return the input unchanged if it is not JSON. */
export function prettyJSON(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2)
  } catch {
    return s
  }
}

export function isJSON(s: string): boolean {
  try {
    JSON.parse(s)
    return true
  } catch {
    return false
  }
}

export function formatCost(c: number): string {
  if (!c) return '$0'
  if (c < 0.01) return '$' + c.toFixed(6)
  return '$' + c.toFixed(4)
}

export function formatNumber(n: number): string {
  return n.toLocaleString()
}

/** Compact token counts: 1234 → 1.2k, 1_500_000 → 1.5M. */
export function formatCompact(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + 'k'
  return (n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 0) + 'M'
}

export function formatDuration(ms: number): string {
  if (!ms) return '—'
  if (ms < 1000) return ms + 'ms'
  if (ms < 60_000) return (ms / 1000).toFixed(2) + 's'
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m ${s}s`
}

export function formatBytes(n: number): string {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / (1024 * 1024)).toFixed(2) + ' MB'
}

export function formatTime(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

export function formatTimeFull(iso: string): string {
  return new Date(iso).toLocaleString()
}

/** Short relative time, e.g. "12s ago", "5m ago", "3h ago", "2d ago". */
export function formatRelative(iso: string): string {
  const then = new Date(iso).getTime()
  const diff = Date.now() - then
  if (diff < 0) return 'just now'
  const s = Math.floor(diff / 1000)
  if (s < 60) return s + 's ago'
  const m = Math.floor(s / 60)
  if (m < 60) return m + 'm ago'
  const h = Math.floor(m / 60)
  if (h < 24) return h + 'h ago'
  const d = Math.floor(h / 24)
  return d + 'd ago'
}

export function statusClass(code: number): string {
  if (code >= 200 && code < 300) return 'text-success'
  if (code >= 400 && code < 500) return 'text-warning'
  return 'text-destructive'
}
