import { CircleAlert } from 'lucide-react'

import { HealthBadge } from '@/components/health-badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Plan, PlanState } from '@/gen/ezdr/portal/v1/portal_pb'
import { planClient } from '@/lib/api'
import { formatBytes, formatDuration, formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'

// PlanStatusCard shows an active plan's replication health.
export function PlanStatusCard({ plan }: { plan: Plan }) {
  const { data } = usePoll(() => planClient.getPlanStatus({ id: plan.id }), 15_000)
  if (plan.state !== PlanState.ACTIVE || !data?.health) return null
  const h = data.health
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          Replication status <HealthBadge state={h.state} />
        </CardTitle>
        <CardDescription>{h.message}</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <div className="grid grid-cols-3 gap-4 text-sm">
          <div>
            <div className="text-muted-foreground">RPO age</div>
            <div className="font-medium">{h.lastReplicationAt ? formatDuration(h.rpoAgeSeconds) : '—'}</div>
          </div>
          <div>
            <div className="text-muted-foreground">Alert threshold</div>
            <div className="font-medium">{formatDuration(h.rpoAlertSeconds)}</div>
          </div>
          <div>
            <div className="text-muted-foreground">Last replication</div>
            <div className="font-medium">{h.lastReplicationAt ? formatRelative(h.lastReplicationAt) : 'not yet'}</div>
          </div>
        </div>
        {data.errors.length > 0 && (
          <div className="grid gap-1 text-sm text-destructive">
            {data.errors.map((e) => (
              <div key={e} className="flex gap-1 break-words">
                <CircleAlert className="mt-0.5 size-4 shrink-0" /> {e}
              </div>
            ))}
          </div>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Dataset</TableHead>
              <TableHead>Newest replicated snapshot</TableHead>
              <TableHead>Age</TableHead>
              <TableHead>Last transfer</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.datasets.map((d) => (
              <TableRow key={d.dataset}>
                <TableCell className="font-mono text-xs">{d.dataset}</TableCell>
                <TableCell className="font-mono text-xs">{d.latestSnapshot || <span className="text-muted-foreground">none yet</span>}</TableCell>
                <TableCell className="text-xs">{d.latestSnapshotAt ? formatDuration(d.ageSeconds) : '—'}</TableCell>
                <TableCell className="text-xs">
                  {d.state || '—'}
                  {d.bytesExpected > 0n && ` · ${formatBytes(d.bytesReplicated)} of ${formatBytes(d.bytesExpected)}`}
                  {d.error && <div className="text-destructive">{d.error}</div>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}
