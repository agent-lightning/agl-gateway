import { useCallback, useEffect, useState } from 'react'
import {
  AlertTriangle,
  ChevronLeft,
  ChevronRight,
  RefreshCw,
} from 'lucide-react'

import { api } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { handleError } from '@/lib/handle-error'
import { useProviders } from '@/hooks/useProviders'
import { applyTimeQuery, presetQuery, type TimeQuery } from '@/lib/timerange'
import {
  formatCompact,
  formatCost,
  formatDuration,
  formatRelative,
  formatTimeFull,
  statusClass,
} from '@/lib/format'
import type { LogsResponse, RequestLog } from '@/lib/types'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { Skeleton } from '@/components/ui/skeleton'
import { TimeRangePicker } from '@/components/TimeRangePicker'
import { LogDrawer } from '@/components/LogDrawer'
import { cn } from '@/lib/utils'

const PAGE_SIZES = [25, 50, 100, 250]

export function LogsTab() {
  const { forget } = useAuth()
  const { providers } = useProviders()

  const [timeQuery, setTimeQuery] = useState<TimeQuery>(() => presetQuery('24h'))
  const [provider, setProvider] = useState('all')
  const [limit, setLimit] = useState(100)
  const [offset, setOffset] = useState(0)

  const [data, setData] = useState<LogsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [selected, setSelected] = useState<RequestLog | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const params = new URLSearchParams()
      params.set('limit', String(limit))
      if (offset) params.set('offset', String(offset))
      if (provider !== 'all') params.set('provider', provider)
      applyTimeQuery(params, timeQuery)
      setData(await api<LogsResponse>('GET', `/admin/logs?${params}`))
    } catch (e) {
      handleError(e, forget)
    } finally {
      setLoading(false)
    }
  }, [limit, offset, provider, timeQuery, forget])

  useEffect(() => {
    load()
  }, [load])

  // Filter changes reset to the first page.
  function changeTime(q: TimeQuery) {
    setOffset(0)
    setTimeQuery(q)
  }
  function changeProvider(p: string) {
    setOffset(0)
    setProvider(p)
  }
  function changeLimit(n: number) {
    setOffset(0)
    setLimit(n)
  }

  const logs = data?.logs ?? []
  const hasMore = data?.has_more ?? false
  const rangeStart = logs.length ? offset + 1 : 0
  const rangeEnd = offset + logs.length

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardContent className="flex flex-wrap items-end gap-3">
          <TimeRangePicker onChange={changeTime} />

          <div className="flex flex-col gap-1.5">
            <Label className="text-muted-foreground text-xs">Provider</Label>
            <Select value={provider} onValueChange={changeProvider}>
              <SelectTrigger className="w-[160px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All providers</SelectItem>
                {providers.map((p) => (
                  <SelectItem key={p.name} value={p.name}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex flex-col gap-1.5">
            <Label className="text-muted-foreground text-xs">Page size</Label>
            <Select
              value={String(limit)}
              onValueChange={(v) => changeLimit(Number(v))}
            >
              <SelectTrigger className="w-[90px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {PAGE_SIZES.map((n) => (
                  <SelectItem key={n} value={String(n)}>
                    {n}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex-1" />
          <Button variant="outline" onClick={load} disabled={loading}>
            <RefreshCw className={cn('size-4', loading && 'animate-spin')} />
            Refresh
          </Button>
        </CardContent>
      </Card>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Time</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>Provider</TableHead>
                <TableHead>Model</TableHead>
                <TableHead>API</TableHead>
                <TableHead className="text-center">Status</TableHead>
                <TableHead className="text-right">Tries</TableHead>
                <TableHead className="text-right">TTFT</TableHead>
                <TableHead className="text-right">Dur</TableHead>
                <TableHead className="text-right">In</TableHead>
                <TableHead className="text-right">Out</TableHead>
                <TableHead className="text-right">Cache</TableHead>
                <TableHead className="text-right pr-4">Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? (
                Array.from({ length: 8 }).map((_, i) => (
                  <TableRow key={i}>
                    <TableCell colSpan={13} className="px-4">
                      <Skeleton className="h-6 w-full" />
                    </TableCell>
                  </TableRow>
                ))
              ) : logs.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={13}
                    className="text-muted-foreground py-12 text-center"
                  >
                    No requests logged for this filter.
                  </TableCell>
                </TableRow>
              ) : (
                logs.map((l) => (
                  <TableRow
                    key={l.id}
                    onClick={() => setSelected(l)}
                    className="cursor-pointer"
                  >
                    <TableCell
                      className="text-muted-foreground pl-4 text-xs"
                      title={formatTimeFull(l.created_at)}
                    >
                      {formatRelative(l.created_at)}
                    </TableCell>
                    <TableCell className="max-w-[140px] truncate font-medium">
                      {l.key_name || '—'}
                    </TableCell>
                    <TableCell>{l.provider}</TableCell>
                    <TableCell className="max-w-[200px] truncate font-mono text-xs">
                      {l.model || '—'}
                      {l.mapped_model && l.mapped_model !== l.model && (
                        <span className="text-muted-foreground">
                          {' '}
                          →{l.mapped_model}
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="text-muted-foreground text-xs">
                      {l.api_type || '—'}
                    </TableCell>
                    <TableCell className="text-center">
                      <span className="inline-flex items-center gap-1">
                        <span
                          className={cn(
                            'tabular font-medium',
                            statusClass(l.status_code),
                          )}
                        >
                          {l.status_code || '—'}
                        </span>
                        {(l.error || l.assemble_error) && (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <AlertTriangle className="text-destructive size-3.5" />
                            </TooltipTrigger>
                            <TooltipContent className="max-w-sm">
                              {l.error || l.assemble_error}
                            </TooltipContent>
                          </Tooltip>
                        )}
                      </span>
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {l.attempts}
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground text-right text-xs">
                      {l.ttft_ms ? formatDuration(l.ttft_ms) : '—'}
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground text-right text-xs">
                      {formatDuration(l.duration_ms)}
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {formatCompact(l.input_tokens)}
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {formatCompact(l.output_tokens)}
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground text-right text-xs">
                      {l.cache_read_tokens || l.cache_write_tokens
                        ? `${formatCompact(l.cache_read_tokens)}/${formatCompact(
                            l.cache_write_tokens,
                          )}`
                        : '—'}
                    </TableCell>
                    <TableCell className="tabular pr-4 text-right">
                      {formatCost(l.cost)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className="flex items-center justify-between px-1">
        <span className="text-muted-foreground text-sm">
          {rangeStart > 0
            ? `Showing ${rangeStart}–${rangeEnd}`
            : 'No results'}
        </span>
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            disabled={offset === 0 || loading}
            onClick={() => setOffset(Math.max(0, offset - limit))}
          >
            <ChevronLeft className="size-4" /> Prev
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={!hasMore || loading}
            onClick={() => setOffset(offset + limit)}
          >
            Next <ChevronRight className="size-4" />
          </Button>
        </div>
      </div>

      <LogDrawer
        logId={selected?.id ?? null}
        preview={selected}
        open={!!selected}
        onClose={() => setSelected(null)}
      />
    </div>
  )
}
