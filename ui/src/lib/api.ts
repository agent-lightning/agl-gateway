// API client for the master-key control plane. The portal is served under /portal/ but the
// admin API lives at the site root (/admin/*), so all requests use absolute paths and work
// regardless of the portal's base path.

let masterKey = ''

export function setMasterKey(key: string) {
  masterKey = key
}

export function getMasterKey() {
  return masterKey
}

/** Thrown on a 401 so the UI can drop back to the login screen. */
export class AuthError extends Error {
  constructor(message = 'invalid master key') {
    super(message)
    this.name = 'AuthError'
  }
}

/** Thrown for any other non-2xx response, carrying the upstream message and status. */
export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

function authHeaders(hasBody: boolean): HeadersInit {
  const h: Record<string, string> = { Authorization: `Bearer ${masterKey}` }
  if (hasBody) h['Content-Type'] = 'application/json'
  return h
}

async function errorMessage(r: Response): Promise<string> {
  try {
    const body = await r.json()
    return body?.error?.message ?? body?.error ?? `request failed (${r.status})`
  } catch {
    return `request failed (${r.status})`
  }
}

export async function api<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const r = await fetch(path, {
    method,
    headers: authHeaders(body !== undefined),
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (r.status === 401) throw new AuthError(await errorMessage(r))
  if (!r.ok) throw new ApiError(await errorMessage(r), r.status)
  if (r.status === 204) return undefined as T
  return r.json() as Promise<T>
}

/**
 * Stream the NDJSON events of POST /admin/test, invoking onEvent for each line. The signal
 * aborts the underlying fetch so a running test can be cancelled.
 */
export async function streamTest(
  body: unknown,
  onEvent: (ev: import('./types').TestEvent) => void,
  signal: AbortSignal,
): Promise<void> {
  const r = await fetch('/admin/test', {
    method: 'POST',
    headers: authHeaders(true),
    body: JSON.stringify(body),
    signal,
  })
  if (r.status === 401) throw new AuthError(await errorMessage(r))
  if (!r.ok) throw new ApiError(await errorMessage(r), r.status)
  if (!r.body) return

  const reader = r.body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buf += dec.decode(value, { stream: true })
    let nl: number
    while ((nl = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, nl).trim()
      buf = buf.slice(nl + 1)
      if (line) onEvent(JSON.parse(line))
    }
  }
}
