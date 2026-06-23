import { useCallback, useEffect, useState } from 'react'
import { Coins, Hash, RefreshCw, Boxes } from 'lucide-react'

import { api } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { handleError } from '@/lib/handle-error'
import { applyTimeQuery, presetQuery, type TimeQuery } from '@/lib/timerange'
import {
  formatCompact,
  formatCost,
  formatNumber,
} from '@/lib/format'
import type { Stat } from '@/lib/types'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Skeleton } from '@/components/ui/skeleton'
import { TimeRangePicker } from '@/components/TimeRangePicker'
import { cn } from '@/lib/utils'

export function StatsTab() {
  const { forget } = useAuth()
  const [timeQuery, setTimeQuery] = useState<TimeQuery>(() => presetQuery('24h'))
  const [stats, setStats] = useState<Stat[]>([])
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const params = new URLSearchParams()
      applyTimeQuery(params, timeQuery)
      const qs = params.toString()
      setStats(await api<Stat[]>('GET', `/admin/stats${qs ? `?${qs}` : ''}`))
    } catch (e) {
      handleError(e, forget)
    } finally {
      setLoading(false)
    }
  }, [timeQuery, forget])

  useEffect(() => {
    load()
  }, [load])

  const totals = stats.reduce(
    (a, s) => ({
      requests: a.requests + s.requests,
      input: a.input + s.input_tokens,
      output: a.output + s.output_tokens,
      cacheR: a.cacheR + s.cache_read_tokens,
      cacheW: a.cacheW + s.cache_write_tokens,
      cost: a.cost + s.cost,
    }),
    { requests: 0, input: 0, output: 0, cacheR: 0, cacheW: 0, cost: 0 },
  )

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardContent className="flex flex-wrap items-end gap-3">
          <TimeRangePicker onChange={setTimeQuery} />
          <div className="flex-1" />
          <Button variant="outline" onClick={load} disabled={loading}>
            <RefreshCw className={cn('size-4', loading && 'animate-spin')} />
            Refresh
          </Button>
        </CardContent>
      </Card>

      <div className="grid gap-4 sm:grid-cols-3">
        <SummaryCard
          icon={<Coins className="size-4" />}
          label="Total cost"
          value={formatCost(totals.cost)}
          loading={loading}
        />
        <SummaryCard
          icon={<Hash className="size-4" />}
          label="Requests"
          value={formatNumber(totals.requests)}
          loading={loading}
        />
        <SummaryCard
          icon={<Boxes className="size-4" />}
          label="Tokens (in / out)"
          value={`${formatCompact(totals.input)} / ${formatCompact(totals.output)}`}
          loading={loading}
        />
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Key</TableHead>
                <TableHead>Model</TableHead>
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">Input</TableHead>
                <TableHead className="text-right">Output</TableHead>
                <TableHead className="text-right">Cache R</TableHead>
                <TableHead className="text-right">Cache W</TableHead>
                <TableHead className="text-right pr-4">Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? (
                Array.from({ length: 5 }).map((_, i) => (
                  <TableRow key={i}>
                    <TableCell colSpan={8} className="px-4">
                      <Skeleton className="h-6 w-full" />
                    </TableCell>
                  </TableRow>
                ))
              ) : stats.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={8}
                    className="text-muted-foreground py-12 text-center"
                  >
                    No usage recorded for this period.
                  </TableCell>
                </TableRow>
              ) : (
                stats.map((s, i) => (
                  <TableRow key={`${s.api_key_id}-${s.model}-${i}`}>
                    <TableCell className="pl-4 font-medium">
                      {s.key_name || (
                        <span className="text-muted-foreground">
                          #{s.api_key_id}
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {s.model || '—'}
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {formatNumber(s.requests)}
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {formatNumber(s.input_tokens)}
                    </TableCell>
                    <TableCell className="tabular text-right">
                      {formatNumber(s.output_tokens)}
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground text-right">
                      {formatNumber(s.cache_read_tokens)}
                    </TableCell>
                    <TableCell className="tabular text-muted-foreground text-right">
                      {formatNumber(s.cache_write_tokens)}
                    </TableCell>
                    <TableCell className="tabular pr-4 text-right font-medium">
                      {formatCost(s.cost)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
            {stats.length > 0 && (
              <TableFooter>
                <TableRow>
                  <TableCell className="pl-4 font-semibold">Total</TableCell>
                  <TableCell>
                    <Badge variant="muted">{stats.length} rows</Badge>
                  </TableCell>
                  <TableCell className="tabular text-right font-semibold">
                    {formatNumber(totals.requests)}
                  </TableCell>
                  <TableCell className="tabular text-right">
                    {formatNumber(totals.input)}
                  </TableCell>
                  <TableCell className="tabular text-right">
                    {formatNumber(totals.output)}
                  </TableCell>
                  <TableCell className="tabular text-right">
                    {formatNumber(totals.cacheR)}
                  </TableCell>
                  <TableCell className="tabular text-right">
                    {formatNumber(totals.cacheW)}
                  </TableCell>
                  <TableCell className="tabular pr-4 text-right font-semibold">
                    {formatCost(totals.cost)}
                  </TableCell>
                </TableRow>
              </TableFooter>
            )}
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

function SummaryCard({
  icon,
  label,
  value,
  loading,
}: {
  icon: React.ReactNode
  label: string
  value: string
  loading: boolean
}) {
  return (
    <Card>
      <CardContent className="flex items-center gap-4">
        <div className="bg-primary/10 text-primary flex size-10 items-center justify-center rounded-lg">
          {icon}
        </div>
        <div className="flex flex-col gap-1">
          <span className="text-muted-foreground text-xs">{label}</span>
          {loading ? (
            <Skeleton className="h-6 w-24" />
          ) : (
            <span className="tabular text-xl font-semibold">{value}</span>
          )}
        </div>
      </CardContent>
    </Card>
  )
}
