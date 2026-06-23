import { createContext, useContext } from 'react'

export interface AuthState {
  /** The active master key (empty when locked). */
  masterKey: string
  /** Replace the master key and re-probe the control plane. */
  connect: (key: string) => void
  /** Clear the master key and return to the login screen. */
  forget: () => void
}

export const AuthContext = createContext<AuthState | null>(null)

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
