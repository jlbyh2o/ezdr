import { TriangleAlert } from 'lucide-react'
import { Link } from 'react-router'

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
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { Host } from '@/gen/ezdr/portal/v1/portal_pb'
import { hostClient } from '@/lib/api'
import { formatDateTime, formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { AddHostDialog } from '@/pages/add-host-dialog'
import { PageHeader } from '@/pages/layout'

export function HostsPage() {
  const { data, error, reload } = usePoll(async () => (await hostClient.listHosts({})).hosts, 10_000)

  return (
    <>
      <PageHeader title="Hosts" description="Proxmox VE hosts enrolled in this portal.">
        <AddHostDialog onCreated={() => void reload()} />
      </PageHeader>
      <ErrorAlert message={error} />
      {data && data.length === 0 && (
        <p className="py-12 text-center text-muted-foreground">No hosts yet. Use “Add host” to enroll your first one.</p>
      )}
      {data && data.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Host</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Guests</TableHead>
              <TableHead>Tunnel address</TableHead>
              <TableHead>Proxmox VE</TableHead>
              <TableHead>Client</TableHead>
              <TableHead>Enrolled</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.map((h) => (
              <TableRow key={h.id}>
                <TableCell className="font-medium">
                  <Link to={`/hosts/${h.id}`} className="hover:underline">
                    {h.hostname}
                  </Link>
                  {h.duplicateMachineId && (
                    <div className="flex items-center gap-1 text-xs text-amber-600">
                      <TriangleAlert className="size-3" /> Same machine ID as another host; remove the stale one.
                    </div>
                  )}
                </TableCell>
                <TableCell>
                  {h.online ? (
                    <Badge>Online</Badge>
                  ) : (
                    <Badge variant="secondary" title={`Last seen ${formatDateTime(h.lastSeenAt)}`}>
                      Offline · {formatRelative(h.lastSeenAt)}
                    </Badge>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {!h.hasInventory ? (
                    <span className="text-muted-foreground">—</span>
                  ) : (
                    <>
                      {h.guestCount}
                      {h.guestsNotReady > 0 && (
                        <span className="text-destructive"> · {h.guestsNotReady} not ready</span>
                      )}
                    </>
                  )}
                </TableCell>
                <TableCell className="font-mono text-xs">{h.tunnelAddress}</TableCell>
                <TableCell className="text-xs">{pveShort(h.pveVersion)}</TableCell>
                <TableCell className="text-xs">{h.clientVersion}</TableCell>
                <TableCell className="text-xs">{formatDateTime(h.enrolledAt)}</TableCell>
                <TableCell className="text-right">
                  <RemoveHostButton host={h} onRemoved={() => void reload()} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
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
