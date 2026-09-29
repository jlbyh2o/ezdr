import { CircleAlert } from 'lucide-react'

import { HealthBadge } from '@/components/health-badge'
import { RPO } from '@/components/rpo'
import { StatusBadge } from '@/components/status-badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Plan, PlanState } from '@/gen/ezdr/portal/v1/portal_pb'
import { failoverClient, planClient } from '@/lib/api'
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
            <div className="text-muted-foreground">
              <RPO /> age
            </div>
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

// PlanStateCard explains a plan that isn't replicating normally: paused,
// failing over, failed over, or failing back.
export function PlanStateCard({ plan, drHostname }: { plan: Plan; drHostname?: string }) {
  const failover = plan.state === PlanState.FAILING_OVER || plan.state === PlanState.FAILED_OVER
  const { data: fo } = usePoll(
    () => (failover ? failoverClient.getFailover({ planId: plan.id }) : Promise.resolve(undefined)),
    failover ? 5000 : undefined,
  )
  const { data: fb } = usePoll(
    () => (plan.state === PlanState.FAILING_BACK ? failoverClient.getFailback({ planId: plan.id }) : Promise.resolve(undefined)),
    plan.state === PlanState.FAILING_BACK ? 5000 : undefined,
  )
  const current = (steps: { name: string; status: string }[] = []) =>
    steps.find((s) => s.status === 'running')?.name ?? steps.find((s) => s.status === 'pending')?.name

  switch (plan.state) {
    case PlanState.PAUSED:
      return (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              Replication status <StatusBadge tone="neutral">Paused</StatusBadge>
            </CardTitle>
            <CardDescription>
              Snapshots and replication are stopped on both hosts. Replicas, snapshots, and zrepl's bookmarks are kept, so resuming continues
              incrementally.
            </CardDescription>
          </CardHeader>
        </Card>
      )
    case PlanState.FAILING_OVER:
    case PlanState.FAILING_BACK: {
      const back = plan.state === PlanState.FAILING_BACK
      const step = back ? current(fb?.failback?.steps) : current(fo?.failover?.steps)
      return (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              {back ? 'Failback' : 'Failover'} in progress{' '}
              <StatusBadge tone="info" pulse>
                {back ? 'Failing back' : 'Failing over'}
              </StatusBadge>
            </CardTitle>
            <CardDescription>
              {step ? `Now: ${step}.` : 'Starting.'} Use {back ? 'Failback' : 'Failover'} status above to follow it and confirm the result.
            </CardDescription>
          </CardHeader>
        </Card>
      )
    }
    case PlanState.FAILED_OVER: {
      const f = fo?.failover
      const running = f?.guests.filter((g) => g.status === 'running').length ?? 0
      return (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              Failed over <StatusBadge tone="warning">Running on {drHostname ?? 'the DR host'}</StatusBadge>
            </CardTitle>
            <CardDescription>
              The guests run on the DR host and are locked on the primary. Replication is stopped. Failing back copies the changes made at the
              DR site back to the primary.
            </CardDescription>
          </CardHeader>
          {f && (
            <CardContent>
              <dl className="grid gap-4 text-sm sm:grid-cols-3">
                <div>
                  <dt className="text-muted-foreground">Failed over</dt>
                  <dd className="font-medium">
                    {formatRelative(f.completedAt ?? f.startedAt)}
                    <span className="font-normal text-muted-foreground">
                      {' '}
                      · {f.breakGlass ? 'break-glass' : f.planned ? 'planned' : 'unplanned'}
                      {f.startedBy && `, by ${f.startedBy}`}
                    </span>
                  </dd>
                </div>
                <div>
                  <dt className="text-muted-foreground">Guests running</dt>
                  <dd className="font-medium">
                    {running} of {f.guests.length}
                  </dd>
                </div>
                <div className="min-w-0">
                  <dt className="text-muted-foreground">Final snapshot</dt>
                  <dd className="truncate font-mono text-xs leading-5">{f.snapshot || '—'}</dd>
                </div>
              </dl>
            </CardContent>
          )}
        </Card>
      )
    }
    default:
      return null
  }
}
