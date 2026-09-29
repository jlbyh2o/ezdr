import { Badge } from '@/components/ui/badge'
import type { Tone } from '@/lib/status'

// StatusBadge shows a status with its color and a dot, so the label, not
// just the color, carries the meaning.
export function StatusBadge({
  tone,
  children,
  title,
  pulse,
}: {
  tone: Tone
  children: React.ReactNode
  title?: string
  pulse?: boolean
}) {
  return (
    <Badge variant={tone} title={title}>
      <span className={`size-1.5 rounded-full bg-current ${pulse ? 'animate-pulse' : ''}`} aria-hidden />
      {children}
    </Badge>
  )
}
