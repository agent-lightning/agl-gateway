import { useEffect, useState } from 'react'
import { AlertTriangle, Loader2 } from 'lucide-react'

import { api } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { handleError } from '@/lib/handle-error'
import type { RequestLog } from '@/lib/types'
import {
  base64ByteLength,
  decodeBase64,
  formatBytes,
  formatCost,
  formatDuration,
  formatNumber,
  formatTimeFull,
  isJSON,
  prettyJSON,
  statusClass,
} from '@/lib/format'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { CopyButton } from '@/components/CopyButton'
import { cn } from '@/lib/utils'

interface Props {
  logId: number | null
  /** Row data already loaded in the table; shown immediately while payloads load. */
  preview: RequestLog | null
  open: boolean
  onClose: () => void
}

export function LogDrawer({ logId, preview, open, onClose }: Props) {
  const { forget } = useAuth()
  const [full, setFull] = useState<RequestLog | null>(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (!open || logId == null) return
    let cancelled = false
    setFull(null)
    setLoading(true)
    api<RequestLog>('GET', `/admin/logs/${logId}`)
      .then((l) => {
        if (!cancelled) setFull(l)
      })
      .catch((e) => handleError(e, forget))
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [open, logId, forget])

  const log = full ?? preview

  return (
    <Sheet open={open} onOpenChange={(o) => !o && onClose()}>
      <SheetContent side="right" className="w-full gap-0 sm:max-w-4xl lg:max-w-6xl">
        {log && (
          <>
            <SheetHeader className="pb-4">
              <div className="flex items-center gap-2">
                <SheetTitle className="font-mono text-base">
                  #{log.id}
                </SheetTitle>
                <span className={cn('font-mono text-sm', statusClass(log.status_code))}>
                  {log.status_code || '—'}
                </span>
                {log.streaming && <Badge variant="secondary">stream</Badge>}
                {log.api_type && <Badge variant="outline">{log.api_type}</Badge>}
                {loading && (
                  <Loader2 className="text-muted-foreground size-4 animate-spin" />
                )}
              </div>
              <SheetDescription title={formatTimeFull(log.created_at)}>
                {log.provider} · {log.model}
                {log.mapped_model && log.mapped_model !== log.model && (
                  <> → {log.mapped_model}</>
                )}{' '}
                · {formatTimeFull(log.created_at)}
              </SheetDescription>
            </SheetHeader>

            <ScrollArea className="h-[calc(100dvh-7rem)]">
              <div className="flex flex-col gap-5 px-6 pb-10">
                <MetaGrid log={log} />

                {(log.error || log.assemble_error) && (
                  <ErrorPanel log={log} />
                )}

                <PayloadSection
                  title="Request body"
                  b64={full?.raw_request}
                  truncated={log.raw_request_truncated}
                  loading={loading && !full}
                  contentType={log.request_content_type}
                />
                <PayloadSection
                  title="Response body"
                  b64={full?.raw_response}
                  truncated={log.raw_response_truncated}
                  loading={loading && !full}
                  contentType={log.response_content_type}
                />
                {(full?.assembled_response ||
                  log.assembled_response_truncated) && (
                  <PayloadSection
                    title="Assembled response"
                    subtitle="Streaming chunks reassembled into a single body"
                    b64={full?.assembled_response}
                    truncated={log.assembled_response_truncated}
                    loading={loading && !full}
                  />
                )}
              </div>
            </ScrollArea>
          </>
        )}
      </SheetContent>
    </Sheet>
  )
}

function MetaItem({
  label,
  value,
  className,
}: {
  label: string
  value: React.ReactNode
  className?: string
}) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-muted-foreground text-xs">{label}</span>
      <span className={cn('tabular text-sm', className)}>{value}</span>
    </div>
  )
}

function MetaGrid({ log }: { log: RequestLog }) {
  return (
    <div className="bg-muted/40 grid grid-cols-2 gap-4 rounded-lg border p-4 sm:grid-cols-3">
      <MetaItem label="Key" value={log.key_name || '—'} className="font-sans" />
      <MetaItem label="Attempts" value={log.attempts} />
      <MetaItem label="Cost" value={formatCost(log.cost)} />
      <MetaItem
        label="TTFT"
        value={log.ttft_ms ? formatDuration(log.ttft_ms) : '—'}
      />
      <MetaItem label="Duration" value={formatDuration(log.duration_ms)} />
      <MetaItem label="Stream" value={log.streaming ? 'yes' : 'no'} />
      <MetaItem label="Input tokens" value={formatNumber(log.input_tokens)} />
      <MetaItem label="Output tokens" value={formatNumber(log.output_tokens)} />
      <MetaItem
        label="Cache R / W"
        value={`${formatNumber(log.cache_read_tokens)} / ${formatNumber(
          log.cache_write_tokens,
        )}`}
      />
    </div>
  )
}

function ErrorPanel({ log }: { log: RequestLog }) {
  return (
    <div className="border-destructive/40 bg-destructive/10 flex flex-col gap-2 rounded-lg border p-4">
      <div className="text-destructive flex items-center gap-2 text-sm font-medium">
        <AlertTriangle className="size-4" /> Error detail
      </div>
      {log.error && (
        <pre className="text-destructive/90 max-h-48 overflow-auto font-mono text-xs whitespace-pre-wrap">
          {log.error}
        </pre>
      )}
      {log.assemble_error && (
        <div className="text-muted-foreground text-xs">
          <span className="font-medium">assemble error:</span>{' '}
          <span className="font-mono">{log.assemble_error}</span>
        </div>
      )}
    </div>
  )
}

function PayloadSection({
  title,
  subtitle,
  b64,
  truncated,
  loading,
  contentType,
}: {
  title: string
  subtitle?: string
  b64?: string
  truncated: boolean
  loading: boolean
  contentType?: string
}) {
  const [pretty, setPretty] = useState(true)
  const [wrap, setWrap] = useState(true)
  const raw = b64 ? decodeBase64(b64) : ''
  const json = raw && isJSON(raw)
  const text = json && pretty ? prettyJSON(raw) : raw
  const bytes = b64 ? base64ByteLength(b64) : 0

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <h3 className="text-sm font-semibold">{title}</h3>
        {bytes > 0 && (
          <span className="text-muted-foreground text-xs">
            {formatBytes(bytes)}
          </span>
        )}
        {truncated && <Badge variant="warning">truncated</Badge>}
        <div className="flex-1" />
        {raw && (
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={() => setWrap((w) => !w)}
          >
            {wrap ? 'Nowrap' : 'Wrap'}
          </Button>
        )}
        {json && (
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={() => setPretty((p) => !p)}
          >
            {pretty ? 'Raw' : 'Pretty'}
          </Button>
        )}
        {raw && <CopyButton text={raw} />}
      </div>
      {subtitle && (
        <p className="text-muted-foreground -mt-1 text-xs">{subtitle}</p>
      )}
      {loading ? (
        <div className="text-muted-foreground bg-muted/30 rounded-lg border p-4 text-sm">
          Loading payload…
        </div>
      ) : raw ? (
        <pre
          className={cn(
            'bg-muted/30 max-h-[28rem] overflow-auto rounded-lg border p-3 font-mono text-xs leading-relaxed',
            wrap ? 'break-words whitespace-pre-wrap' : 'whitespace-pre',
          )}
        >
          {text}
        </pre>
      ) : (
        <div className="text-muted-foreground bg-muted/20 rounded-lg border border-dashed p-4 text-sm">
          {truncated
            ? 'Payload was too large and was not stored.'
            : 'Not captured.'}
          {contentType && (
            <span className="ml-1 opacity-70">({contentType})</span>
          )}
        </div>
      )}
    </div>
  )
}
