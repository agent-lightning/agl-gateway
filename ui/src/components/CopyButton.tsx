import { useState } from 'react'
import { Check, Copy } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

interface Props {
  text: string
  label?: string
  className?: string
}

/** A small copy-to-clipboard button that flips to a check for a moment after copying. */
export function CopyButton({ text, label, className }: Props) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1400)
    } catch {
      // Clipboard may be unavailable on insecure origins; fail silently.
    }
  }

  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      onClick={copy}
      className={cn('h-7 gap-1.5 px-2 text-xs', className)}
    >
      {copied ? (
        <Check className="size-3.5 text-success" />
      ) : (
        <Copy className="size-3.5" />
      )}
      {label ?? (copied ? 'Copied' : 'Copy')}
    </Button>
  )
}
