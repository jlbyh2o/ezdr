import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type DnsRecordSwitch } from '@/gen/ezdr/portal/v1/portal_pb'

// DnsTable lists the DNS records a failover or failback switches.
export function DnsTable({ records }: { records: DnsRecordSwitch[] }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>DNS record</TableHead>
          <TableHead>Production</TableHead>
          <TableHead>Failover</TableHead>
          <TableHead>Status</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {records.map((r) => (
          <TableRow key={`${r.name}/${r.type}`}>
            <TableCell className="font-mono text-xs">
              {r.name} {r.type}
            </TableCell>
            <TableCell className="font-mono text-xs">{r.productionValue}</TableCell>
            <TableCell className="font-mono text-xs">{r.failoverValue}</TableCell>
            <TableCell className="text-xs">
              {r.status}
              {r.detail && <div className="text-muted-foreground">{r.detail}</div>}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
