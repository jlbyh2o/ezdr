import { ErrorAlert } from '@/components/error-alert'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { auditClient } from '@/lib/api'
import { formatDateTime } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'

export function AuditPage() {
  const { data, error } = usePoll(async () => (await auditClient.listAuditEvents({ limit: 200 })).events, 30_000)

  return (
    <>
      <PageHeader title="Audit log" description="Sign-ins, tokens, enrollments, and host changes, newest first." />
      <ErrorAlert message={error} />
      {data && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Time</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Actor</TableHead>
              <TableHead>Details</TableHead>
              <TableHead>Source</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.map((e, i) => (
              <TableRow key={i}>
                <TableCell className="text-xs whitespace-nowrap">{formatDateTime(e.time)}</TableCell>
                <TableCell className="font-mono text-xs">{e.action}</TableCell>
                <TableCell>{e.actor || <span className="text-muted-foreground">host</span>}</TableCell>
                <TableCell className="text-xs">{e.detail}</TableCell>
                <TableCell className="font-mono text-xs">{e.sourceAddress}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </>
  )
}
