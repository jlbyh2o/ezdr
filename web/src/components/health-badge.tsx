import { Badge } from '@/components/ui/badge'
import { HealthState } from '@/gen/ezdr/portal/v1/portal_pb'

const labels: Record<HealthState, { text: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' }> = {
  [HealthState.UNSPECIFIED]: { text: '—', variant: 'outline' },
  [HealthState.NONE]: { text: '—', variant: 'outline' },
  [HealthState.HEALTHY]: { text: 'Healthy', variant: 'default' },
  [HealthState.SYNCING]: { text: 'Initial sync', variant: 'secondary' },
  [HealthState.LAGGING]: { text: 'Lagging', variant: 'destructive' },
  [HealthState.FAILING]: { text: 'Failing', variant: 'destructive' },
  [HealthState.UNKNOWN]: { text: 'Unknown', variant: 'secondary' },
}

export function HealthBadge({ state, title }: { state?: HealthState; title?: string }) {
  const l = labels[state ?? HealthState.UNSPECIFIED]
  if (l.text === '—') return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant={l.variant} title={title}>
      {l.text}
    </Badge>
  )
}
