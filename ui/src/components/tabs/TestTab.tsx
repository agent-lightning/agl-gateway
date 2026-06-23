import { useRef, useState } from 'react'
import { CheckCircle2, Play, Square, XCircle } from 'lucide-react'

import { streamTest } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { handleError } from '@/lib/handle-error'
import { useProviders } from '@/hooks/useProviders'
import { formatDuration, statusClass } from '@/lib/format'
import type { ProbeResult, TestEvent } from '@/lib/types'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { cn } from '@/lib/utils'

export function TestTab() {
  const { forget } = useAuth()
  const { providers } = useProviders()

  const [provider, setProvider] = useState('all')
  const [exclude, setExclude] = useState('gpt-image*')
  const [maxTokens, setMaxTokens] = useState(16)
  const [concurrency, setConcurrency] = useState(8)
  const [stream, setStream] = useState(false)

  const [running, setRunning] = useState(false)
  const [results, setResults] = useState<ProbeResult[]>([])
  const [stats, setStats] = useState({
    total: 0,
    done: 0,
    passed: 0,
    failed: 0,
    skipped: 0,
  })
  const [finished, setFinished] = useState(false)
  const abortRef = useRef<AbortController | null>(null)

  async function run() {
    setRunning(true)
    setFinished(false)
    setResults([])
    setStats({ total: 0, done: 0, passed: 0, failed: 0, skipped: 0 })
    const ctrl = new AbortController()
    abortRef.current = ctrl

    const onEvent = (ev: TestEvent) => {
      if (ev.type === 'start') {
        setStats((s) => ({
          ...s,
          total: ev.total ?? 0,
          skipped: ev.skipped ?? 0,
        }))
      } else if (ev.type === 'result' && ev.result) {
        const r = ev.result
        setResults((prev) => [...prev, r])
        setStats((s) => ({
          ...s,
          done: s.done + 1,
          passed: s.passed + (r.ok ? 1 : 0),
          failed: s.failed + (r.ok ? 0 : 1),
        }))
      } else if (ev.type === 'done') {
        setStats((s) => ({
          ...s,
          passed: ev.passed ?? s.passed,
          failed: ev.failed ?? s.failed,
          skipped: ev.skipped ?? s.skipped,
        }))
        setFinished(true)
      } else if (ev.type === 'error') {
        throw new Error(ev.message || 'test failed')
      }
    }

    try {
      await streamTest(
        {
          provider: provider === 'all' ? '' : provider,
          exclude,
          max_tokens: maxTokens || 16,
          concurrency: concurrency || 8,
          stream,
        },
        onEvent,
        ctrl.signal,
      )
    } catch (e) {
      if (!ctrl.signal.aborted) handleError(e, forget)
    } finally {
      setRunning(false)
      abortRef.current = null
    }
  }

  function cancel() {
    abortRef.current?.abort()
    setRunning(false)
  }

  const pct = stats.total ? Math.round((stats.done / stats.total) * 100) : 0

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardContent className="flex flex-col gap-4">
          <div className="grid gap-4 md:grid-cols-4">
            <div className="flex flex-col gap-1.5">
              <Label className="text-muted-foreground text-xs">Provider</Label>
              <Select
                value={provider}
                onValueChange={setProvider}
                disabled={running}
              >
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">All providers</SelectItem>
                  {providers.map((p) => (
                    <SelectItem key={p.name} value={p.name}>
                      {p.name} ({p.models.length})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex flex-col gap-1.5 md:col-span-2">
              <Label className="text-muted-foreground text-xs">
                Exclude (globs, comma-separated)
              </Label>
              <Input
                value={exclude}
                onChange={(e) => setExclude(e.target.value)}
                placeholder="gpt-image*,*-audio"
                disabled={running}
              />
            </div>
            <div className="flex gap-3">
              <div className="flex flex-col gap-1.5">
                <Label className="text-muted-foreground text-xs">
                  Max tokens
                </Label>
                <Input
                  type="number"
                  value={maxTokens}
                  onChange={(e) => setMaxTokens(Number(e.target.value))}
                  className="w-24"
                  disabled={running}
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label className="text-muted-foreground text-xs">
                  Concurrency
                </Label>
                <Input
                  type="number"
                  value={concurrency}
                  onChange={(e) => setConcurrency(Number(e.target.value))}
                  className="w-24"
                  disabled={running}
                />
              </div>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-4">
            <label className="flex cursor-pointer items-center gap-2 text-sm">
              <Checkbox
                checked={stream}
                onCheckedChange={(v) => setStream(!!v)}
                disabled={running}
              />
              Send <code className="font-mono text-xs">stream: true</code>
            </label>
            <div className="flex-1" />
            {running ? (
              <Button variant="destructive" onClick={cancel}>
                <Square className="size-4" /> Stop
              </Button>
            ) : (
              <Button onClick={run}>
                <Play className="size-4" /> Run test
              </Button>
            )}
          </div>

          <p className="text-muted-foreground text-xs">
            Sends one small request per model through the gateway (same as the
            modelcheck CLI). This creates real request-log entries.
          </p>
        </CardContent>
      </Card>

      {(running || results.length > 0 || finished) && (
        <Card>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-3">
              <div className="bg-muted h-2 flex-1 overflow-hidden rounded-full">
                <div
                  className={cn(
                    'h-full rounded-full transition-all',
                    finished && stats.failed === 0
                      ? 'bg-success'
                      : finished
                        ? 'bg-warning'
                        : 'bg-primary',
                  )}
                  style={{ width: `${finished ? 100 : pct}%` }}
                />
              </div>
              <span className="tabular text-muted-foreground text-sm">
                {stats.done}/{stats.total}
              </span>
            </div>
            <div className="flex flex-wrap gap-2 text-sm">
              <Badge variant="success">{stats.passed} passed</Badge>
              {stats.failed > 0 && (
                <Badge variant="destructive">{stats.failed} failed</Badge>
              )}
              {stats.skipped > 0 && (
                <Badge variant="muted">{stats.skipped} skipped</Badge>
              )}
            </div>
          </CardContent>
        </Card>
      )}

      {results.length > 0 && (
        <Card>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4">Result</TableHead>
                  <TableHead>Provider</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Endpoint</TableHead>
                  <TableHead className="text-center">Status</TableHead>
                  <TableHead className="text-right">Tries</TableHead>
                  <TableHead className="text-right">Dur</TableHead>
                  <TableHead className="pr-4">Detail</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {results.map((r, i) => (
                  <TableRow key={`${r.provider}-${r.model}-${i}`}>
                    <TableCell className="pl-4">
                      {r.ok ? (
                        <span className="text-success inline-flex items-center gap-1 text-xs font-medium">
                          <CheckCircle2 className="size-4" /> ok
                        </span>
                      ) : (
                        <span className="text-destructive inline-flex items-center gap-1 text-xs font-medium">
                          <XCircle className="size-4" /> fail
                        </span>
                      )}
                    </TableCell>
                    <TableCell>{r.provider}</TableCell>
                    <TableCell className="font-mono text-xs">{r.model}</TableCell>
                    <TableCell className="text-muted-foreground font-mono text-xs">
                      {r.endpoint}
                    </TableCell>
                    <TableCell className="text-center">
                      <span
                        className={cn('tabular', statusClass(r.status))}
                      >
                        {r.status || '—'}
                      </span>
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {r.attempts || '—'}
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground text-right text-xs">
                      {formatDuration(r.duration_ms)}
                    </TableCell>
                    <TableCell
                      className={cn(
                        'max-w-[320px] truncate pr-4 text-xs',
                        r.ok ? 'text-muted-foreground' : 'text-destructive',
                      )}
                      title={r.detail}
                    >
                      {r.detail}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
