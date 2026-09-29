import { Plus } from 'lucide-react'
import { Link } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { HealthBadge } from '@/components/health-badge'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { planClient } from '@/lib/api'
import { describeInterval } from '@/lib/retention'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'
import { StateBadge } from '@/pages/plan-actions'

export function PlansPage() {
  const { data, error } = usePoll(async () => (await planClient.listPlans({})).plans, 30_000)

  return (
    <>
      <PageHeader title="DR plans" description="What each primary host replicates to its DR host, and how.">
        <Button render={<Link to="/plans/new" />}>
          <Plus /> New plan
        </Button>
      </PageHeader>
      <ErrorAlert message={error} />
      {data && data.length === 0 && (
        <p className="py-12 text-center text-muted-foreground">No plans yet. Create one to protect your guests.</p>
      )}
      {data && data.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Plan</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Health</TableHead>
              <TableHead>Primary → DR</TableHead>
              <TableHead>Guests</TableHead>
              <TableHead>Snapshots</TableHead>
              <TableHead>Validation</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.map((p) => (
              <TableRow key={p.id}>
                <TableCell className="font-medium">
                  <Link to={`/plans/${p.id}`} className="hover:underline">
                    {p.name}
                  </Link>
                </TableCell>
                <TableCell>
                  <StateBadge state={p.state} pending={p.pendingChanges} />
                </TableCell>
                <TableCell>
                  <HealthBadge state={p.health?.state} title={p.health?.message} />
                </TableCell>
                <TableCell className="text-sm">
                  {p.primaryHostname} → {p.drHostname}
                </TableCell>
                <TableCell>{p.guestCount}</TableCell>
                <TableCell className="text-sm">{describeInterval(p.intervalSeconds)}</TableCell>
                <TableCell>
                  {p.errorCount > 0 ? (
                    <Badge variant="destructive">
                      {p.errorCount} error{p.errorCount === 1 ? '' : 's'}
                    </Badge>
                  ) : (
                    <Badge variant="success">Valid</Badge>
                  )}
                  {p.warningCount > 0 && (
                    <span className="ml-2 text-xs text-warning-foreground">
                      {p.warningCount} warning{p.warningCount === 1 ? '' : 's'}
                    </span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </>
  )
}
