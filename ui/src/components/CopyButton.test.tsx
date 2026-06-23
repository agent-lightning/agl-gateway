import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import { CopyButton } from '@/components/CopyButton'

describe('CopyButton', () => {
  it('copies the text and flips the label to "Copied"', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    const user = userEvent.setup()
    // userEvent.setup() stubs navigator.clipboard; override writeText to observe it.
    vi.spyOn(navigator.clipboard, 'writeText').mockImplementation(writeText)

    render(<CopyButton text="sk-gw-abc" />)
    expect(screen.getByRole('button', { name: /copy/i })).toBeInTheDocument()

    await user.click(screen.getByRole('button'))
    expect(writeText).toHaveBeenCalledWith('sk-gw-abc')
    expect(await screen.findByText('Copied')).toBeInTheDocument()
  })

  it('does not throw when the clipboard API rejects', async () => {
    const user = userEvent.setup()
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(
      new Error('denied'),
    )
    render(<CopyButton text="x" />)
    await user.click(screen.getByRole('button'))
    // Still rendered, no unhandled rejection.
    expect(screen.getByRole('button')).toBeInTheDocument()
  })
})
