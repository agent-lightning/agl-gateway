import { toast } from 'sonner'

import { AuthError } from './api'

/**
 * Centralized error handling for control-plane calls: a 401 drops the session back to the
 * login screen; everything else surfaces as a toast. Returns the message shown.
 */
export function handleError(e: unknown, forget?: () => void): string {
  if (e instanceof AuthError) {
    toast.error('Session expired — re-enter the master key')
    forget?.()
    return e.message
  }
  const msg = e instanceof Error ? e.message : 'request failed'
  toast.error(msg)
  return msg
}
