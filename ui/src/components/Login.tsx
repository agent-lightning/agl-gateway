import { useState, type FormEvent } from 'react'
import { KeyRound, Loader2, ShieldAlert } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

interface Props {
  onConnect: (key: string) => void
  error?: string
  connecting?: boolean
}

/** Full-screen, master-key-gated entry point. Nothing else renders until connected. */
export function Login({ onConnect, error, connecting }: Props) {
  const [key, setKey] = useState('')

  function submit(e: FormEvent) {
    e.preventDefault()
    const trimmed = key.trim()
    if (trimmed) onConnect(trimmed)
  }

  return (
    <div className="relative flex min-h-dvh items-center justify-center overflow-hidden p-6">
      {/* Ambient brand glow backdrop (primary red + accent orange). */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 -z-10"
        style={{
          background:
            'radial-gradient(60rem 40rem at 50% -10%, oklch(0.62 0.158 20 / 0.2), transparent 70%), radial-gradient(40rem 30rem at 110% 110%, oklch(0.749 0.151 53 / 0.14), transparent 70%)',
        }}
      />
      <div className="w-full max-w-md">
        <div className="mb-8 flex flex-col items-center text-center">
          <img
            src="/portal/favicon.png"
            alt=""
            className="mb-4 size-14 drop-shadow-[0_0_24px_oklch(0.749_0.151_53_/_0.55)]"
          />
          <h1 className="text-2xl font-semibold tracking-tight">agl-gateway</h1>
          <p className="text-muted-foreground mt-1 text-sm">
            Control plane · keys, logs, usage &amp; model checks
          </p>
        </div>

        <div className="bg-card/70 rounded-xl border p-6 shadow-xl backdrop-blur">
          <form onSubmit={submit} className="flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <label
                htmlFor="master"
                className="text-muted-foreground flex items-center gap-2 text-sm font-medium"
              >
                <KeyRound className="size-4" /> Master key
              </label>
              <Input
                id="master"
                type="password"
                autoFocus
                autoComplete="off"
                placeholder="Enter your master key"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                className="h-11 text-base"
                aria-invalid={!!error}
              />
            </div>

            {error && (
              <div className="text-destructive flex items-center gap-2 text-sm">
                <ShieldAlert className="size-4 shrink-0" />
                <span>{error}</span>
              </div>
            )}

            <Button
              type="submit"
              size="lg"
              disabled={connecting || !key.trim()}
              className="h-11 w-full text-base"
            >
              {connecting && <Loader2 className="size-4 animate-spin" />}
              {connecting ? 'Connecting…' : 'Connect'}
            </Button>
          </form>
        </div>

        <p className="text-muted-foreground mt-6 text-center text-xs">
          The master key is held in this browser only and sent as a bearer token to{' '}
          <code className="font-mono">/admin</code>.
        </p>
      </div>
    </div>
  )
}
