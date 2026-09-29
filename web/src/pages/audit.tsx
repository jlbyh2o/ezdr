import { Search } from 'lucide-react'
import { useState } from 'react'

import { ErrorAlert } from '@/components/error-alert'
import { Segmented } from '@/components/segmented'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { AuditEvent } from '@/gen/ezdr/portal/v1/portal_pb'
import { auditClient } from '@/lib/api'
import { formatDateTime, formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'

type Category = 'all' | 'operations' | 'plans' | 'hosts' | 'access' | 'settings'

// category groups audit actions for filtering.
function category(action: string): Exclude<Category, 'all'> {
  if (/^plan\.(failover|failed_over|failback|failed_back|test|takeover|guest_locked)/.test(action)) return 'operations'
  if (action.startsWith('plan.')) return 'plans'
  if (/^(host|guest|token)\./.test(action)) return 'hosts'
  if (/^(auth|setup)\./.test(action)) return 'access'
  return 'settings'
}

const categories: { value: Category; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'operations', label: 'Operations' },
  { value: 'plans', label: 'Plans' },
  { value: 'hosts', label: 'Hosts and guests' },
  { value: 'access', label: 'Sign-ins' },
  { value: 'settings', label: 'Settings' },
]

const pageSizes = [200, 1000]

export function AuditPage() {
  const [size, setSize] = useState(0)
  const { data, error } = usePoll(async () => (await auditClient.listAuditEvents({ limit: pageSizes[size] })).events, 30_000)
  const [filter, setFilter] = useState<Category>('all')
  const [query, setQuery] = useState('')
  const q = query.trim().toLowerCase()
  const matches = (e: AuditEvent) =>
    (filter === 'all' || category(e.action) === filter) &&
    (!q || [e.action, e.actor, e.detail, e.sourceAddress].some((f) => f.toLowerCase().includes(q)))
  const events = data?.filter(matches) ?? []

  return (
    <>
      <PageHeader title="Audit log" description="Everything done through the portal and by its operations, newest first." />
      <ErrorAlert message={error} />
      <div className="flex flex-wrap items-center gap-3">
        <Segmented label="Kind" value={filter} onChange={setFilter} options={categories} />
        <div className="relative ml-auto w-full sm:w-72">
          <Search className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search actions, people, details" className="pl-8" />
        </div>
      </div>
      {data && events.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No matching events.</p>}
      {events.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Time</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>By</TableHead>
              <TableHead>Details</TableHead>
              <TableHead>Source</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((e, i) => (
              <TableRow key={i}>
                <TableCell className="text-xs whitespace-nowrap" title={formatDateTime(e.time)}>
                  {formatRelative(e.time)}
                </TableCell>
                <TableCell className="font-mono text-xs">{e.action}</TableCell>
                <TableCell className="text-sm">{e.actor || <span className="text-muted-foreground">host</span>}</TableCell>
                <TableCell className="text-xs">{e.detail || <span className="text-muted-foreground">{e.target}</span>}</TableCell>
                <TableCell className="font-mono text-xs">
                  {e.sourceAddress && e.sourceAddress !== 'invalid IP' ? e.sourceAddress : <span className="text-muted-foreground">—</span>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {data && size < pageSizes.length - 1 && data.length >= pageSizes[size] && (
        <div>
          <Button variant="outline" size="sm" onClick={() => setSize(size + 1)}>
            Load older events
          </Button>
        </div>
      )}
    </>
  )
}
