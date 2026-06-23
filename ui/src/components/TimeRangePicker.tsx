import { useState } from 'react'
import { CalendarClock } from 'lucide-react'

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import {
  DEFAULT_PRESET,
  TIME_PRESETS,
  localInputToRFC3339,
  presetQuery,
  type TimeQuery,
} from '@/lib/timerange'

interface Props {
  onChange: (query: TimeQuery) => void
}

/**
 * Preset + custom date-range selector. Presets emit a relative `since`; the custom mode
 * emits an RFC3339 `since`/`until` pair so a fixed historical window can be queried.
 */
export function TimeRangePicker({ onChange }: Props) {
  const [preset, setPreset] = useState(DEFAULT_PRESET)
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')

  function pick(value: string) {
    setPreset(value)
    if (value !== 'custom') onChange(presetQuery(value))
  }

  function applyCustom() {
    onChange({
      since: localInputToRFC3339(from),
      until: localInputToRFC3339(to),
    })
  }

  return (
    <div className="flex flex-wrap items-end gap-2">
      <div className="flex flex-col gap-1.5">
        <Label className="text-muted-foreground text-xs">Time range</Label>
        <Select value={preset} onValueChange={pick}>
          <SelectTrigger className="w-[170px]">
            <CalendarClock className="size-3.5 opacity-60" />
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {TIME_PRESETS.map((p) => (
              <SelectItem key={p.value} value={p.value}>
                {p.label}
              </SelectItem>
            ))}
            <SelectItem value="custom">Custom range…</SelectItem>
          </SelectContent>
        </Select>
      </div>

      {preset === 'custom' && (
        <>
          <div className="flex flex-col gap-1.5">
            <Label className="text-muted-foreground text-xs">From</Label>
            <Input
              type="datetime-local"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              className="w-[210px]"
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label className="text-muted-foreground text-xs">To</Label>
            <Input
              type="datetime-local"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className="w-[210px]"
            />
          </div>
          <Button variant="secondary" onClick={applyCustom}>
            Apply
          </Button>
        </>
      )}
    </div>
  )
}
