import { toast } from 'sonner'

import { AuthError } from './api'

/** Capitalize the first letter and end with a period so terse backend messages
 * (e.g. "log not found") read as proper sentences in a toast. */
function asSentence(msg: string): string {
  const t = msg.trim()
  if (!t) return 'The request could not be completed.'
  const capitalized = t.charAt(0).toUpperCase() + t.slice(1)
  return /[.!?]$/.test(capitalized) ? capitalized : `${capitalized}.`
}

/**
 * Centralized error handling for control-plane calls: a 401 drops the session back to the
 * login screen; everything else surfaces as a toast. Returns the message shown.
 */
export function handleError(e: unknown, forget?: () => void): string {
  if (e instanceof AuthError) {
    toast.error('Session expired', {
      description: 'Please re-enter the master key to continue.',
    })
    forget?.()
    return e.message
  }
  const msg = e instanceof Error ? e.message : 'request failed'
  toast.error('Request failed', { description: asSentence(msg) })
  return msg
}
