import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import { Login } from '@/components/Login'

describe('Login', () => {
  it('disables Connect until a key is entered, then submits the trimmed key', async () => {
    const onConnect = vi.fn()
    const user = userEvent.setup()
    render(<Login onConnect={onConnect} />)

    const button = screen.getByRole('button', { name: /connect/i })
    expect(button).toBeDisabled()

    await user.type(screen.getByLabelText(/master key/i), '  mk-secret  ')
    expect(button).toBeEnabled()

    await user.click(button)
    expect(onConnect).toHaveBeenCalledExactlyOnceWith('mk-secret')
  })

  it('shows an error message and marks the input invalid', () => {
    render(<Login onConnect={vi.fn()} error="invalid master key" />)
    expect(screen.getByText('invalid master key')).toBeInTheDocument()
    expect(screen.getByLabelText(/master key/i)).toHaveAttribute(
      'aria-invalid',
      'true',
    )
  })

  it('shows a connecting state and disables the button', () => {
    render(<Login onConnect={vi.fn()} connecting />)
    expect(
      screen.getByRole('button', { name: /connecting/i }),
    ).toBeDisabled()
  })
})
