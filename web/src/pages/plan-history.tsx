import type { Timestamp } from '@bufbuild/protobuf/wkt'
import { ArrowLeftRight, ChevronRight, FlaskConical, RotateCcw, Workflow } from 'lucide-react'
import { useState } from 'react'
import { Link } from 'react-router'

import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type Plan, TakeoverState } from '@/gen/ezdr/portal/v1/portal_pb'
import { failoverClient, planClient, testClient } from '@/lib/api'
import { formatBytes, formatDateTime, formatRelative, toDate } from '@/lib/format'
import { failbackStates, failoverStates, operationPath, takeoverStates, testStates, verdictLabel } from '@/lib/operations'
import type { Tone } from '@/lib/status'
import { usePoll } from '@/lib/use-poll'

type Row = {
  key: string
  icon: React.ComponentType<{ className?: string }>
  kind: string
  startedAt?: Timestamp
  startedBy?: string
  look: { label: string; tone: Tone; active?: boolean }
  detail: string
  to: string
}

// loadHistory gathers a plan's operations, newest first.
async function loadHistory(planId: string): Promise<Row[]> {
  const [tests, failovers, failbacks, takeover] = await Promise.all([
    testClient.listTests({ planId }),
    failoverClient.listFailovers({ planId }),
    failoverClient.listFailbacks({ planId }),
    planClient.getTakeover({ id: planId }),
  ])
  const rows: Row[] = []
  for (const t of tests.tests) {
    const running = t.guests.filter((g) => g.status === 'running').length
    rows.push({
      key: `test:${t.id}`,
      icon: FlaskConical,
      kind: 'Test failover',
      startedAt: t.startedAt,
      startedBy: t.startedBy,
      look: testStates[t.state],
      detail: `${running} of ${t.guests.length} guests ran · from ${formatDateTime(t.snapshotAt)} · ${verdictLabel[t.verdict]}`,
      to: operationPath.test(planId, t.id),
    })
  }
  for (const f of failovers.failovers) {
    rows.push({
      key: `failover:${f.id}`,
      icon: ArrowLeftRight,
      kind: f.breakGlass ? 'Break-glass failover' : f.planned ? 'Planned failover' : 'Unplanned failover',
      startedAt: f.startedAt,
      startedBy: f.startedBy,
      look: failoverStates[f.state],
      detail: f.snapshot ? `Final snapshot ${f.snapshot}` : '',
      to: operationPath.failover(planId, f.id),
    })
  }
  for (const f of failbacks.failbacks) {
    const copied = f.rounds.reduce((n, r) => n + r.bytes, 0n)
    rows.push({
      key: `failback:${f.id}`,
      icon: RotateCcw,
      kind: 'Failback',
      startedAt: f.startedAt,
      startedBy: f.startedBy,
      look: failbackStates[f.state],
      detail: f.rounds.length > 0 ? `${f.rounds.length} ${f.rounds.length === 1 ? 'copy' : 'copies'}, ${formatBytes(copied)}` : '',
      to: operationPath.failback(planId, f.id),
    })
  }
  const t = takeover.takeover
  const started = t?.steps.find((s) => s.status !== 'pending' && s.updatedAt)?.updatedAt
  // A takeover that only ran its preflight never started.
  if (t && started && t.state !== TakeoverState.READY && t.state !== TakeoverState.BLOCKED) {
    rows.push({
      key: 'takeover',
      icon: Workflow,
      kind: 'zrepl takeover',
      startedAt: started,
      look: takeoverStates[t.state],
      detail: t.error,
      to: operationPath.takeover(planId),
    })
  }
  return rows.sort((a, b) => (toDate(b.startedAt)?.getTime() ?? 0) - (toDate(a.startedAt)?.getTime() ?? 0))
}

const shown = 8

// PlanHistoryCard lists the plan's takeover, test failovers, failovers, and
// failbacks, each linking to its page.
export function PlanHistoryCard({ plan }: { plan: Plan }) {
  const { data } = usePoll(() => loadHistory(plan.id), 15000)
  const [all, setAll] = useState(false)
  const rows = data ?? []
  const visible = all ? rows : rows.slice(0, shown)
  return (
    <Card>
      <CardHeader>
        <CardTitle>History</CardTitle>
        <CardDescription>This plan's takeover, test failovers, failovers, and failbacks.</CardDescription>
      </CardHeader>
      <CardContent>
        {data && rows.length === 0 && <p className="text-sm text-muted-foreground">Nothing yet.</p>}
        {rows.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Operation</TableHead>
                <TableHead>Started</TableHead>
                <TableHead>Result</TableHead>
                <TableHead>Details</TableHead>
                <TableHead className="w-8" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.map((r) => (
                <TableRow key={r.key} className="group">
                  <TableCell>
                    <Link to={r.to} className="flex items-center gap-2 font-medium hover:underline">
                      <r.icon className="size-4 text-muted-foreground" /> {r.kind}
                    </Link>
                  </TableCell>
                  <TableCell className="text-xs">
                    <span title={formatDateTime(r.startedAt)}>{formatRelative(r.startedAt)}</span>
                    {r.startedBy && <span className="text-muted-foreground"> by {r.startedBy}</span>}
                  </TableCell>
                  <TableCell>
                    <StatusBadge tone={r.look.tone} pulse={r.look.tone === 'info'}>
                      {r.look.label}
                    </StatusBadge>
                  </TableCell>
                  <TableCell className="max-w-80 truncate text-xs text-muted-foreground" title={r.detail}>
                    {r.detail}
                  </TableCell>
                  <TableCell>
                    <Link to={r.to} aria-label={`Open ${r.kind}`} className="text-muted-foreground group-hover:text-foreground">
                      <ChevronRight className="size-4" />
                    </Link>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {rows.length > shown && (
          <Button variant="ghost" size="sm" className="mt-2" onClick={() => setAll(!all)}>
            {all ? 'Show fewer' : `Show all ${rows.length}`}
          </Button>
        )}
      </CardContent>
    </Card>
  )
}
