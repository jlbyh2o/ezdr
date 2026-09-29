import { BellOff, CheckCircle2 } from 'lucide-react'
import { useState } from 'react'
import { Link } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { Segmented } from '@/components/segmented'
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { AlertSeverity } from '@/gen/ezdr/portal/v1/portal_pb'
import { alertClient, hostClient, planClient } from '@/lib/api'
import { formatDateTime, formatDuration, formatRelative, toDate } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'

type Filter = 'firing' | 'resolved' | 'all'

export function AlertsPage() {
  const { data, error } = usePoll(async () => {
    const [alerts, plans, hosts] = await Promise.all([
      alertClient.listAlerts({ limit: 200 }),
      planClient.listPlans({}),
      hostClient.listHosts({}),
    ])
    return {
      alerts: alerts.alerts,
      planNames: new Map(plans.plans.map((p) => [p.id, p.name])),
      hostNames: new Map(hosts.hosts.map((h) => [h.id, h.hostname])),
    }
  }, 30_000)
  const [filter, setFilter] = useState<Filter>('firing')
  const alerts = data?.alerts ?? []
  const firing = alerts.filter((a) => !a.resolvedAt)
  const shown = filter === 'firing' ? firing : filter === 'resolved' ? alerts.filter((a) => a.resolvedAt) : alerts

  return (
    <>
      <PageHeader title="Alerts" description="Problems EZDR noticed, and when they cleared. Choose where alerts are sent in Settings." />
      <ErrorAlert message={error} />
      {data && (
        <>
          <Segmented
            label="Show"
            value={filter}
            onChange={setFilter}
            options={[
              { value: 'firing', label: 'Firing', count: firing.length },
              { value: 'resolved', label: 'Resolved', count: alerts.length - firing.length },
              { value: 'all', label: 'All', count: alerts.length },
            ]}
          />
          {shown.length === 0 ? (
            <div className="grid justify-items-center gap-2 py-16 text-center text-sm text-muted-foreground">
              {filter === 'firing' ? <CheckCircle2 className="size-8 text-success" /> : <BellOff className="size-8" />}
              {filter === 'firing' ? 'Nothing is firing.' : 'No alerts here.'}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Alert</TableHead>
                  <TableHead>About</TableHead>
                  <TableHead>Fired</TableHead>
                  <TableHead>Resolved</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((a) => {
                  const fired = toDate(a.firedAt)
                  const resolved = toDate(a.resolvedAt)
                  const lasted = fired && resolved ? Math.round((resolved.getTime() - fired.getTime()) / 1000) : 0
                  return (
                    <TableRow key={`${a.key}:${a.firedAt?.seconds}`} className="align-top">
                      <TableCell>
                        <div className="grid justify-items-start gap-1">
                          {a.resolvedAt ? (
                            <StatusBadge tone="neutral">Resolved</StatusBadge>
                          ) : (
                            <StatusBadge tone="destructive" pulse>
                              Firing
                            </StatusBadge>
                          )}
                          {a.severity === AlertSeverity.CRITICAL ? (
                            <Badge variant="destructive">Critical</Badge>
                          ) : (
                            <Badge variant="warning">Warning</Badge>
                          )}
                        </div>
                      </TableCell>
                      <TableCell>
                        <div className="font-medium">{a.title}</div>
                        <div className="text-xs whitespace-pre-line text-muted-foreground">{a.message}</div>
                      </TableCell>
                      <TableCell className="text-xs">
                        {a.planId && (
                          <Link to={`/plans/${a.planId}`} className="block hover:underline">
                            {data.planNames.get(a.planId) ?? 'plan'}
                          </Link>
                        )}
                        {a.hostId && (
                          <Link to={`/hosts/${a.hostId}`} className="block text-muted-foreground hover:underline">
                            {data.hostNames.get(a.hostId) ?? 'host'}
                          </Link>
                        )}
                      </TableCell>
                      <TableCell className="text-xs whitespace-nowrap" title={formatDateTime(a.firedAt)}>
                        {formatRelative(a.firedAt)}
                      </TableCell>
                      <TableCell className="text-xs whitespace-nowrap" title={a.resolvedAt ? formatDateTime(a.resolvedAt) : undefined}>
                        {a.resolvedAt ? (
                          <>
                            {formatRelative(a.resolvedAt)}
                            <div className="text-muted-foreground">after {formatDuration(lasted)}</div>
                          </>
                        ) : (
                          '—'
                        )}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </>
      )}
    </>
  )
}
