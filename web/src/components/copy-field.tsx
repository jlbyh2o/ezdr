import { Check, Copy } from 'lucide-react'
import { useState } from 'react'

import { Button } from '@/components/ui/button'

// CopyField shows a command or secret with a copy button.
export function CopyField({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    await navigator.clipboard.writeText(value)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="flex items-start gap-2">
      <code className="block min-w-0 flex-1 rounded-md border bg-muted px-3 py-2 font-mono text-xs break-all">
        {value}
      </code>
      <Button variant="outline" size="icon" onClick={copy} aria-label={`Copy ${label}`}>
        {copied ? <Check /> : <Copy />}
      </Button>
    </div>
  )
}
