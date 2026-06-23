import { useCallback, useEffect, useState } from 'react'

import { api } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { handleError } from '@/lib/handle-error'
import type { ProviderInfo } from '@/lib/types'

/**
 * Loads the configured providers and the models each exposes. The backend probes every
 * provider's /v1/models live (bounded to ~15s), so this can take a moment on first load.
 */
export function useProviders() {
  const { forget } = useAuth()
  const [providers, setProviders] = useState<ProviderInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | undefined>()

  const reload = useCallback(async () => {
    setLoading(true)
    try {
      setProviders(await api<ProviderInfo[]>('GET', '/admin/providers'))
      setError(undefined)
    } catch (e) {
      setError(handleError(e, forget))
    } finally {
      setLoading(false)
    }
  }, [forget])

  useEffect(() => {
    reload()
  }, [reload])

  return { providers, loading, error, reload }
}
