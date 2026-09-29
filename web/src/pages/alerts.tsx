import { ErrorAlert } from '@/components/error-alert'
import { StatusBadge } from '@/components/status-badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { alertClient } from '@/lib/api'
import { formatDateTime } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'

export function AlertsPage() {
  const { data, error } = usePoll(async () => (await alertClient.listAlerts({ limit: 200 })).alerts, 30_000)
  return (
    <>
      <PageHeader title="Alerts" description="Firing alerts first, then recently resolved ones. Configure where alerts go in Settings." />
      <ErrorAlert message={error} />
      {data && data.length === 0 && <p className="py-12 text-center text-muted-foreground">No alerts.</p>}
      {data && data.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Status</TableHead>
              <TableHead>Alert</TableHead>
              <TableHead>Fired</TableHead>
              <TableHead>Resolved</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.map((a, i) => (
              <TableRow key={i} className="align-top">
                <TableCell>{a.resolvedAt ? <StatusBadge tone="neutral">Resolved</StatusBadge> : <StatusBadge tone="destructive">Firing</StatusBadge>}</TableCell>
                <TableCell>
                  <div className="font-medium">{a.title}</div>
                  <div className="text-xs whitespace-pre-line text-muted-foreground">{a.message}</div>
                </TableCell>
                <TableCell className="text-xs whitespace-nowrap">{formatDateTime(a.firedAt)}</TableCell>
                <TableCell className="text-xs whitespace-nowrap">{a.resolvedAt ? formatDateTime(a.resolvedAt) : '—'}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </>
  )
}
