import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'

import {
  AuthError,
  api,
  getMasterKey,
  setMasterKey,
  streamTest,
} from '@/lib/api'
import type { TestEvent } from '@/lib/types'

function jsonResponse(body: unknown, status = 200) {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('master key', () => {
  it('stores and returns the key', () => {
    setMasterKey('mk-123')
    expect(getMasterKey()).toBe('mk-123')
  })
})

describe('api', () => {
  beforeEach(() => setMasterKey('mk-test'))
  afterEach(() => vi.restoreAllMocks())

  it('sends the bearer token and returns parsed JSON', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse([{ id: 1 }]))
    const out = await api<{ id: number }[]>('GET', '/admin/keys')
    expect(out).toEqual([{ id: 1 }])
    const [, init] = fetchMock.mock.calls[0]
    expect((init?.headers as Record<string, string>).Authorization).toBe(
      'Bearer mk-test',
    )
  })

  it('serializes a JSON body and sets content-type', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse({ ok: true }, 201))
    await api('POST', '/admin/keys', { name: 'x' })
    const [, init] = fetchMock.mock.calls[0]
    expect(init?.body).toBe('{"name":"x"}')
    expect((init?.headers as Record<string, string>)['Content-Type']).toBe(
      'application/json',
    )
  })

  it('returns undefined for 204 No Content', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(null, { status: 204 }),
    )
    await expect(api('DELETE', '/admin/keys/1')).resolves.toBeUndefined()
  })

  it('throws AuthError on 401', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({ error: { message: 'invalid master key' } }, 401),
    )
    await expect(api('GET', '/admin/keys')).rejects.toBeInstanceOf(AuthError)
  })

  it('throws ApiError with the upstream message on other failures', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({ error: { message: 'name is required' } }, 400),
    )
    await expect(api('POST', '/admin/keys', {})).rejects.toMatchObject({
      name: 'ApiError',
      status: 400,
      message: 'name is required',
    })
  })

  it('falls back to a generic message when the error body is unparseable', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response('<html>502</html>', { status: 502 }),
    )
    await expect(api('GET', '/admin/stats')).rejects.toMatchObject({
      message: 'request failed (502)',
    })
  })
})

describe('streamTest', () => {
  beforeEach(() => setMasterKey('mk-test'))
  afterEach(() => vi.restoreAllMocks())

  function ndjsonResponse(lines: string[]) {
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const enc = new TextEncoder()
        for (const l of lines) controller.enqueue(enc.encode(l))
        controller.close()
      },
    })
    return new Response(stream, { status: 200 })
  }

  it('parses NDJSON events split across chunk boundaries', async () => {
    // Deliberately split a record across two chunks to exercise the line buffer.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      ndjsonResponse([
        '{"type":"start","total":2}\n{"type":"resu',
        'lt","result":{"ok":true}}\n',
        '{"type":"done","passed":1}\n',
      ]),
    )
    const events: TestEvent[] = []
    await streamTest({}, (ev) => events.push(ev), new AbortController().signal)
    expect(events.map((e) => e.type)).toEqual(['start', 'result', 'done'])
    expect(events[0].total).toBe(2)
    expect(events[1].result?.ok).toBe(true)
  })

  it('throws AuthError on 401 before streaming', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({ error: { message: 'invalid master key' } }, 401),
    )
    await expect(
      streamTest({}, () => {}, new AbortController().signal),
    ).rejects.toBeInstanceOf(AuthError)
  })
})
