import { RefreshCw } from 'lucide-react'
import { useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Plan, PlanState } from '@/gen/ezdr/portal/v1/portal_pb'
import { dnsClient, errorMessage } from '@/lib/api'
import { formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'

// PlanDnsCard shows a plan's DNS records as last read from Cloudflare.
export function PlanDnsCard({ plan }: { plan: Plan }) {
  const { data, reload } = usePoll(() => dnsClient.getPlanDnsStatus({ planId: plan.id }), 60_000)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const hasRecords = (plan.appliedSpec?.guests ?? []).some((g) => g.dnsRecords.length > 0)
  if (plan.state === PlanState.DRAFT || !hasRecords) return null
  const st = data?.status
  async function check() {
    setBusy(true)
    setError(undefined)
    try {
      await dnsClient.checkPlanDns({ planId: plan.id })
      await reload()
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between gap-2">
          DNS records
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void check()}>
            <RefreshCw /> Check now
          </Button>
        </CardTitle>
        <CardDescription>
          {st?.checkedAt ? `Checked ${formatRelative(st.checkedAt)}.` : 'Not checked yet.'} Records must match the plan's state: production values
          while it runs at the primary, failover values after a failover switched them.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <ErrorAlert message={error || st?.error} />
        {st && st.records.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Record</TableHead>
                <TableHead>Expected</TableHead>
                <TableHead>In Cloudflare</TableHead>
                <TableHead>TTL</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {st.records.map((r) => (
                <TableRow key={`${r.name}/${r.type}`} className="align-top">
                  <TableCell className="font-mono text-xs">
                    {r.name} {r.type}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{r.expected || '—'}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {r.current || '—'}
                    {r.problems.map((p) => (
                      <div key={p} className={`font-sans ${r.drift ? 'text-destructive' : 'text-amber-700'}`}>
                        {p}
                      </div>
                    ))}
                  </TableCell>
                  <TableCell className="text-xs">{r.proxied ? 'proxied' : r.ttl === 1 ? 'auto' : r.ttl ? `${r.ttl}s` : '—'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  )
}
