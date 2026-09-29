import { StatusBadge } from '@/components/status-badge'
import { HealthState } from '@/gen/ezdr/portal/v1/portal_pb'
import { healthStates } from '@/lib/status'

export function HealthBadge({ state, title }: { state?: HealthState; title?: string }) {
  const h = healthStates[state ?? HealthState.UNSPECIFIED]
  if (!h.label) return <span className="text-muted-foreground">—</span>
  return (
    <StatusBadge tone={h.tone} title={title} pulse={state === HealthState.SYNCING}>
      {h.label}
    </StatusBadge>
  )
}
