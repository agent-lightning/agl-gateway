import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import { AuthContext } from '@/lib/auth'
import type { RequestLog } from '@/lib/types'
import { LogDrawer } from '@/components/LogDrawer'

// The drawer fetches the full log (with payloads) on open; stub the API layer.
const apiMock = vi.fn()
vi.mock('@/lib/api', () => ({ api: (...args: unknown[]) => apiMock(...args) }))

// A long single-line body is the case that overflows horizontally.
const longLine = 'the quick brown fox '.repeat(80)

function makeLog(): RequestLog {
  return {
    id: 7,
    api_key_id: 1,
    key_name: 'app',
    provider: 'mock',
    model: 'gpt-test',
    mapped_model: '',
    request_content_type: 'application/json',
    response_content_type: 'application/json',
    status_code: 200,
    streaming: false,
    attempts: 1,
    ttft_ms: 0,
    duration_ms: 12,
    input_tokens: 1,
    output_tokens: 1,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    cost: 0,
    error: '',
    raw_request: btoa(longLine),
    raw_request_truncated: false,
    raw_response_truncated: false,
    assembled_response_truncated: false,
    created_at: new Date(0).toISOString(),
  }
}

function renderDrawer() {
  const auth = { masterKey: 'mk', connect: vi.fn(), forget: vi.fn() }
  return render(
    <AuthContext.Provider value={auth}>
      <LogDrawer logId={7} preview={makeLog()} open onClose={vi.fn()} />
    </AuthContext.Provider>,
  )
}

/** The <pre> holding a payload, found via its decoded text (awaits the async fetch). */
async function payloadPre(): Promise<HTMLElement> {
  const node = await screen.findByText(longLine.trim(), { exact: false })
  const pre = node.closest('pre')
  if (!pre) throw new Error('payload <pre> not found')
  return pre
}

describe('LogDrawer payload wrapping', () => {
  beforeEach(() => {
    apiMock.mockReset().mockResolvedValue(makeLog())
  })

  it('wraps long lines by default and toggles to nowrap', async () => {
    const user = userEvent.setup()
    renderDrawer()

    // Default: wrapped so content stays visible without horizontal scrolling.
    const pre = await payloadPre()
    expect(pre).toHaveClass('whitespace-pre-wrap')
    expect(pre).not.toHaveClass('whitespace-pre')

    // The request-body section offers a Nowrap toggle.
    const section = pre.closest('div')!.parentElement as HTMLElement
    const toggle = within(section).getByRole('button', { name: /nowrap/i })
    await user.click(toggle)

    expect(await payloadPre()).toHaveClass('whitespace-pre')
    expect(await payloadPre()).not.toHaveClass('whitespace-pre-wrap')
    // The button now offers to switch back to wrap.
    expect(within(section).getByRole('button', { name: /^wrap$/i })).toBeInTheDocument()
  })
})
