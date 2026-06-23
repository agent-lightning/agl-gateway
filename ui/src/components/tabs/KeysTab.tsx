import { useCallback, useEffect, useState } from 'react'
import { Check, Copy, KeyRound, Loader2, Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'

import { api } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { handleError } from '@/lib/handle-error'
import { formatTimeFull, formatRelative } from '@/lib/format'
import type {
  APIKey,
  CreatedKey,
  ProviderOrder,
  ProviderStart,
} from '@/lib/types'
import { useProviders } from '@/hooks/useProviders'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'

export function KeysTab() {
  const { forget } = useAuth()
  const { providers, loading: provLoading } = useProviders()
  const [keys, setKeys] = useState<APIKey[]>([])
  const [loading, setLoading] = useState(true)

  // New-key form state.
  const [name, setName] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [start, setStart] = useState<ProviderStart>('first')
  const [order, setOrder] = useState<ProviderOrder>('round_robin')
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<CreatedKey | null>(null)
  const [copied, setCopied] = useState(false)

  const [pendingDelete, setPendingDelete] = useState<APIKey | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setKeys(await api<APIKey[]>('GET', '/admin/keys'))
    } catch (e) {
      handleError(e, forget)
    } finally {
      setLoading(false)
    }
  }, [forget])

  useEffect(() => {
    load()
  }, [load])

  function toggleProvider(p: string) {
    setSelected((prev) => {
      const next = new Set(prev)
      next.has(p) ? next.delete(p) : next.add(p)
      return next
    })
  }

  async function create() {
    if (!name.trim()) return toast.error('Name is required')
    if (selected.size === 0) return toast.error('Select at least one provider')
    setCreating(true)
    try {
      const key = await api<CreatedKey>('POST', '/admin/keys', {
        name: name.trim(),
        providers: [...selected],
        provider_start: start,
        provider_order: order,
      })
      setCreated(key)
      setCopied(false)
      setName('')
      setSelected(new Set())
      toast.success(`Key “${key.name}” created`)
      load()
    } catch (e) {
      handleError(e, forget)
    } finally {
      setCreating(false)
    }
  }

  async function confirmDelete() {
    if (!pendingDelete) return
    const id = pendingDelete.id
    setPendingDelete(null)
    try {
      await api('DELETE', `/admin/keys/${id}`)
      toast.success('Key deleted')
      load()
    } catch (e) {
      handleError(e, forget)
    }
  }

  async function copyCreated() {
    if (!created) return
    try {
      await navigator.clipboard.writeText(created.key)
      setCopied(true)
      setTimeout(() => setCopied(false), 1600)
    } catch {
      /* ignore */
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Plus className="size-4" /> Create API key
          </CardTitle>
          <CardDescription>
            A gateway key routes to the selected providers. The plaintext key is shown
            once, on creation.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="grid gap-4 md:grid-cols-2">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="kname">Name</Label>
              <Input
                id="kname"
                placeholder="e.g. mobile-app"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="flex gap-4">
              <div className="flex flex-1 flex-col gap-1.5">
                <Label>Start</Label>
                <Select
                  value={start}
                  onValueChange={(v) => setStart(v as ProviderStart)}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="first">first</SelectItem>
                    <SelectItem value="random">random</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="flex flex-1 flex-col gap-1.5">
                <Label>Retry order</Label>
                <Select
                  value={order}
                  onValueChange={(v) => setOrder(v as ProviderOrder)}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="round_robin">round robin</SelectItem>
                    <SelectItem value="random">random</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
          </div>

          <div className="flex flex-col gap-2">
            <Label>Providers</Label>
            {provLoading ? (
              <div className="flex gap-2">
                <Skeleton className="h-8 w-28" />
                <Skeleton className="h-8 w-28" />
              </div>
            ) : providers.length === 0 ? (
              <p className="text-muted-foreground text-sm">
                No providers configured.
              </p>
            ) : (
              <div className="flex flex-wrap gap-2">
                {providers.map((p) => {
                  const on = selected.has(p.name)
                  return (
                    <button
                      key={p.name}
                      type="button"
                      onClick={() => toggleProvider(p.name)}
                      className={cn(
                        'flex items-center gap-2 rounded-lg border px-3 py-2 text-sm transition-colors',
                        on
                          ? 'border-primary bg-primary/10 text-foreground'
                          : 'hover:bg-accent border-input',
                      )}
                    >
                      <Checkbox checked={on} className="pointer-events-none" />
                      <span className="font-medium">{p.name}</span>
                      <span className="text-muted-foreground text-xs">
                        {p.error ? 'unreachable' : `${p.models.length} models`}
                      </span>
                    </button>
                  )
                })}
              </div>
            )}
          </div>

          <div>
            <Button onClick={create} disabled={creating}>
              {creating ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <KeyRound className="size-4" />
              )}
              Create key
            </Button>
          </div>

          {created && (
            <div className="border-success/40 bg-success/10 flex flex-col gap-2 rounded-lg border p-4">
              <div className="text-sm font-medium">
                New key for “{created.name}” — copy it now, it won’t be shown again.
              </div>
              <div className="flex items-center gap-2">
                <code className="bg-background/60 flex-1 overflow-x-auto rounded-md border px-3 py-2 font-mono text-sm">
                  {created.key}
                </code>
                <Button variant="secondary" size="sm" onClick={copyCreated}>
                  {copied ? (
                    <Check className="size-4 text-success" />
                  ) : (
                    <Copy className="size-4" />
                  )}
                  {copied ? 'Copied' : 'Copy'}
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Keys</CardTitle>
          <CardDescription>
            {keys.length} key{keys.length === 1 ? '' : 's'}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-12">ID</TableHead>
                <TableHead>Name</TableHead>
                <TableHead>Prefix</TableHead>
                <TableHead>Providers</TableHead>
                <TableHead>Policy</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="w-12"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? (
                Array.from({ length: 3 }).map((_, i) => (
                  <TableRow key={i}>
                    <TableCell colSpan={7}>
                      <Skeleton className="h-6 w-full" />
                    </TableCell>
                  </TableRow>
                ))
              ) : keys.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={7}
                    className="text-muted-foreground py-8 text-center"
                  >
                    No keys yet — create one above.
                  </TableCell>
                </TableRow>
              ) : (
                keys.map((k) => (
                  <TableRow key={k.id}>
                    <TableCell className="tabular text-muted-foreground">
                      {k.id}
                    </TableCell>
                    <TableCell className="font-medium">{k.name}</TableCell>
                    <TableCell>
                      <code className="font-mono text-xs">{k.prefix}…</code>
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {k.providers.map((p) => (
                          <Badge key={p} variant="secondary">
                            {p}
                          </Badge>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell className="text-muted-foreground text-xs">
                      {k.provider_start} · {k.provider_order}
                    </TableCell>
                    <TableCell
                      className="text-muted-foreground text-xs"
                      title={formatTimeFull(k.created_at)}
                    >
                      {formatRelative(k.created_at)}
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="text-destructive hover:text-destructive size-8"
                        onClick={() => setPendingDelete(k)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Dialog
        open={!!pendingDelete}
        onOpenChange={(o) => !o && setPendingDelete(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete key “{pendingDelete?.name}”?</DialogTitle>
            <DialogDescription>
              Requests using this key will stop working immediately. All of its request
              logs are deleted too. This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setPendingDelete(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={confirmDelete}>
              <Trash2 className="size-4" /> Delete key
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
