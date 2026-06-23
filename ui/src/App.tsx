import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Activity,
  KeyRound,
  LogOut,
  Moon,
  ScrollText,
  Sun,
  TestTube2,
} from 'lucide-react'

import { AuthContext } from '@/lib/auth'
import { AuthError, api, setMasterKey } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { Toaster } from '@/components/ui/sonner'
import { Login } from '@/components/Login'
import { KeysTab } from '@/components/tabs/KeysTab'
import { LogsTab } from '@/components/tabs/LogsTab'
import { StatsTab } from '@/components/tabs/StatsTab'
import { TestTab } from '@/components/tabs/TestTab'

const STORAGE_KEY = 'agl_master'
const THEME_KEY = 'agl_theme'

type Status = 'idle' | 'connecting' | 'connected'

function useTheme() {
  const [dark, setDark] = useState(
    () => (localStorage.getItem(THEME_KEY) ?? 'dark') !== 'light',
  )
  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem(THEME_KEY, dark ? 'dark' : 'light')
  }, [dark])
  return { dark, toggle: () => setDark((d) => !d) }
}

export default function App() {
  const [master, setMaster] = useState(
    () => localStorage.getItem(STORAGE_KEY) ?? '',
  )
  const [status, setStatus] = useState<Status>('idle')
  const [error, setError] = useState<string | undefined>()
  const [version, setVersion] = useState<string>('')
  const { dark, toggle } = useTheme()
  const didInit = useRef(false)

  const connect = useCallback(async (key: string) => {
    setMaster(key)
    setMasterKey(key)
    setError(undefined)
    setStatus('connecting')
    try {
      // A lightweight probe doubles as the credential check.
      await api('GET', '/admin/keys')
      localStorage.setItem(STORAGE_KEY, key)
      setStatus('connected')
    } catch (e) {
      if (e instanceof AuthError) {
        localStorage.removeItem(STORAGE_KEY)
        setStatus('idle')
        setError(e.message)
        return
      }
      // The key was accepted (not a 401); the failure is elsewhere. Let the user in.
      localStorage.setItem(STORAGE_KEY, key)
      setStatus('connected')
    }
  }, [])

  const forget = useCallback(() => {
    setMaster('')
    setMasterKey('')
    localStorage.removeItem(STORAGE_KEY)
    setStatus('idle')
    setError(undefined)
  }, [])

  // Auto-connect once if a key was remembered.
  useEffect(() => {
    if (didInit.current) return
    didInit.current = true
    if (master) connect(master)
  }, [master, connect])

  // Fetch the build version for the header badge.
  useEffect(() => {
    if (status !== 'connected') return
    fetch('/healthz')
      .then((r) => r.json())
      .then((d) => setVersion(d?.version ?? ''))
      .catch(() => {})
  }, [status])

  if (status !== 'connected') {
    return (
      <>
        <Login
          onConnect={connect}
          error={error}
          connecting={status === 'connecting'}
        />
        <Toaster />
      </>
    )
  }

  return (
    <AuthContext.Provider value={{ masterKey: master, connect, forget }}>
      <div className="min-h-dvh">
        <header className="bg-card/60 sticky top-0 z-30 border-b backdrop-blur">
          <div className="mx-auto flex h-14 max-w-[1600px] items-center gap-3 px-4 sm:px-6">
            <img src="/portal/favicon.svg" alt="" className="size-6" />
            <span className="text-[15px] font-semibold tracking-tight">
              agl-gateway
            </span>
            {version && (
              <Badge variant="muted" className="font-mono text-[11px]">
                {version}
              </Badge>
            )}
            <span className="flex items-center gap-1.5">
              <span className="bg-success size-2 animate-pulse rounded-full" />
              <span className="text-muted-foreground hidden text-xs sm:inline">
                connected
              </span>
            </span>
            <div className="flex-1" />
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="ghost" size="icon" onClick={toggle}>
                  {dark ? (
                    <Sun className="size-4" />
                  ) : (
                    <Moon className="size-4" />
                  )}
                </Button>
              </TooltipTrigger>
              <TooltipContent>Toggle theme</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="ghost" size="sm" onClick={forget}>
                  <LogOut className="size-4" /> Lock
                </Button>
              </TooltipTrigger>
              <TooltipContent>Forget master key</TooltipContent>
            </Tooltip>
          </div>
        </header>

        <main className="mx-auto max-w-[1600px] px-4 py-6 sm:px-6">
          <Tabs defaultValue="logs" className="gap-6">
            <TabsList className="h-10">
              <TabsTrigger value="keys" className="px-4">
                <KeyRound className="size-4" /> API Keys
              </TabsTrigger>
              <TabsTrigger value="logs" className="px-4">
                <ScrollText className="size-4" /> Logs
              </TabsTrigger>
              <TabsTrigger value="stats" className="px-4">
                <Activity className="size-4" /> Usage
              </TabsTrigger>
              <TabsTrigger value="test" className="px-4">
                <TestTube2 className="size-4" /> Test models
              </TabsTrigger>
            </TabsList>

            <TabsContent value="keys">
              <KeysTab />
            </TabsContent>
            <TabsContent value="logs">
              <LogsTab />
            </TabsContent>
            <TabsContent value="stats">
              <StatsTab />
            </TabsContent>
            <TabsContent value="test">
              <TestTab />
            </TabsContent>
          </Tabs>
        </main>
      </div>
      <Toaster />
    </AuthContext.Provider>
  )
}
