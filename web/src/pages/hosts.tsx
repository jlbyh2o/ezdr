import { Server, TriangleAlert } from 'lucide-react'
import { Link, useSearchParams } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Segmented } from '@/components/segmented'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { Host } from '@/gen/ezdr/portal/v1/portal_pb'
import { hostClient, planClient } from '@/lib/api'
import { formatDateTime, formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { AddHostDialog } from '@/pages/add-host-dialog'
import { PageHeader } from '@/pages/layout'
import { TokensTable } from '@/pages/tokens'

type Tab = 'hosts' | 'tokens'

export function HostsPage() {
  const [params, setParams] = useSearchParams()
  const tab: Tab = params.get('tab') === 'tokens' ? 'tokens' : 'hosts'
  const { data, error, reload } = usePoll(async () => {
    const [hosts, plans] = await Promise.all([hostClient.listHosts({}), planClient.listPlans({})])
    return { hosts: hosts.hosts, plans: plans.plans }
  }, 10_000)
  const role = (h: Host) => {
    const primary = data?.plans.some((p) => p.primaryHostname === h.hostname)
    const dr = data?.plans.some((p) => p.drHostname === h.hostname)
    return primary && dr ? 'Primary and DR host' : primary ? 'Primary' : dr ? 'DR host' : 'Not in a plan'
  }

  return (
    <>
      <PageHeader title="Hosts" description="Proxmox VE hosts enrolled in this portal, and the tokens that enroll them.">
        <AddHostDialog onCreated={() => void reload()} />
      </PageHeader>
      <Segmented
        label="Show"
        value={tab}
        onChange={(t) => setParams(t === 'tokens' ? { tab: 'tokens' } : {})}
        options={[
          { value: 'hosts', label: 'Hosts', count: data?.hosts.length },
          { value: 'tokens', label: 'Enrollment tokens' },
        ]}
      />
      {tab === 'tokens' ? (
        <TokensTable />
      ) : (
        <>
          <ErrorAlert message={error} />
          {data && data.hosts.length === 0 && (
            <div className="grid justify-items-center gap-2 py-16 text-center text-sm text-muted-foreground">
              <Server className="size-8" />
              No hosts yet. Use “Add host” to enroll a primary host and a DR host.
            </div>
          )}
          {data && data.hosts.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Host</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Guests</TableHead>
                  <TableHead>Proxmox VE</TableHead>
                  <TableHead>Client</TableHead>
                  <TableHead>Last seen</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.hosts.map((h) => (
                  <TableRow key={h.id}>
                    <TableCell>
                      <div className="flex items-center gap-2.5">
                        <Server
                          className={`size-5 shrink-0 ${h.online ? 'text-success' : 'text-destructive'}`}
                          role="img"
                          aria-label={h.online ? 'Online' : 'Offline'}
                        >
                          <title>{h.online ? 'Online' : 'Offline'}</title>
                        </Server>
                        <div className="grid">
                          <Link to={`/hosts/${h.id}`} className="font-medium hover:underline">
                            {h.hostname}
                          </Link>
                          <span className="font-mono text-xs text-muted-foreground">{h.tunnelAddress}</span>
                        </div>
                      </div>
                      {h.duplicateMachineId && (
                        <div className="mt-1 flex items-center gap-1 text-xs text-warning-foreground">
                          <TriangleAlert className="size-3" /> Same machine ID as another host; remove the stale one.
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="text-sm">
                      {role(h)}
                      {h.planCount > 0 && (
                        <div className="text-xs text-muted-foreground">
                          {h.planCount} plan{h.planCount === 1 ? '' : 's'}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="text-sm">
                      {!h.hasInventory ? (
                        <span className="text-muted-foreground">—</span>
                      ) : (
                        <>
                          {h.guestCount}
                          {h.guestsNotReady > 0 && <div className="text-xs text-destructive">{h.guestsNotReady} not ready</div>}
                        </>
                      )}
                    </TableCell>
                    <TableCell className="text-xs">{pveShort(h.pveVersion)}</TableCell>
                    <TableCell className="text-xs">{h.clientVersion || '—'}</TableCell>
                    <TableCell className="text-xs" title={formatDateTime(h.lastSeenAt)}>
                      {h.online ? <span className="text-success">Online now</span> : formatRelative(h.lastSeenAt)}
                    </TableCell>
                    <TableCell className="text-right">
                      <RemoveHostButton host={h} onRemoved={() => void reload()} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}
    </>
  )
}

function pveShort(v: string): string {
  const m = /pve-manager\/([\d.]+)/.exec(v)
  return m ? m[1] : v
}

function RemoveHostButton({ host, onRemoved }: { host: Host; onRemoved: () => void }) {
  async function remove() {
    await hostClient.deleteHost({ id: host.id })
    onRemoved()
  }
  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button variant="ghost" size="sm" />}>Remove</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove {host.hostname}?</AlertDialogTitle>
          <AlertDialogDescription>
            The host is disconnected immediately and can no longer reach the portal. To clean up the host itself,
            run <code>ezdr unenroll</code> on it. To add it back later, enroll it again with a new token.
            {host.planCount > 0 && (
              <strong className="mt-2 block text-destructive">
                {host.planCount} DR plan{host.planCount === 1 ? '' : 's'} using this host will also be deleted.
              </strong>
            )}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={() => void remove()}>
            Remove host
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
