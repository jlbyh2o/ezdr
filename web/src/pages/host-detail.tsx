import { ArrowLeft, CircleAlert, RefreshCw, TriangleAlert } from 'lucide-react'
import { useState } from 'react'
import { Link, useParams } from 'react-router'

import { ErrorAlert } from '@/components/error-alert'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  type Guest,
  GuestType,
  type Inventory,
  Readiness,
  type ZreplJob,
  type ZreplPruneRule,
} from '@/gen/ezdr/inventory/v1/inventory_pb'
import type { GetHostInventoryResponse } from '@/gen/ezdr/portal/v1/portal_pb'
import { errorMessage, hostClient } from '@/lib/api'
import { formatBytes, formatDuration, formatRelative } from '@/lib/format'
import { usePoll } from '@/lib/use-poll'
import { PageHeader } from '@/pages/layout'

export function HostDetailPage() {
  const { id = '' } = useParams()
  const { data, error, reload } = usePoll(() => hostClient.getHostInventory({ hostId: id }), 15_000)
  const [refreshError, setRefreshError] = useState<string>()
  const [refreshing, setRefreshing] = useState(false)

  async function refresh() {
    setRefreshing(true)
    setRefreshError(undefined)
    try {
      await hostClient.refreshInventory({ hostId: id })
      // The host reports within a second or two.
      await new Promise((r) => setTimeout(r, 2000))
      await reload()
    } catch (err) {
      setRefreshError(errorMessage(err))
    } finally {
      setRefreshing(false)
    }
  }

  const host = data?.host
  const inv = data?.inventory
  return (
    <>
      <Link to="/hosts" className="flex items-center gap-1 pt-4 text-sm text-muted-foreground hover:text-foreground">
        <ArrowLeft className="size-4" /> Hosts
      </Link>
      <PageHeader
        title={host?.hostname ?? 'Host'}
        description={
          data?.receivedAt
            ? `Inventory last changed ${formatRelative(data.changedAt)} · checked ${formatRelative(data.receivedAt)}`
            : 'No inventory reported yet.'
        }
      >
        {host && (
          <div className="flex items-center gap-2">
            {host.online ? <StatusBadge tone="success">Online</StatusBadge> : <StatusBadge tone="destructive">Offline</StatusBadge>}
            <Button variant="outline" onClick={() => void refresh()} disabled={!host.online || refreshing}>
              <RefreshCw className={refreshing ? 'animate-spin' : ''} /> Refresh
            </Button>
          </div>
        )}
      </PageHeader>
      <ErrorAlert message={error ?? refreshError} />
      {inv && inv.warnings.length > 0 && (
        <Alert>
          <CircleAlert />
          <AlertTitle>Some inventory could not be collected</AlertTitle>
          <AlertDescription>
            <ul className="list-disc pl-4">
              {inv.warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {inv && <InventoryView inv={inv} guestPlans={data?.guestPlans ?? {}} />}
    </>
  )
}

function InventoryView({ inv, guestPlans }: { inv: Inventory; guestPlans: GuestPlans }) {
  const h = inv.host
  const notReady = inv.guests.filter((g) => !g.ready).length
  return (
    <>
      {h && (
        <Card>
          <CardContent className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-5">
            <Fact label="Proxmox VE" value={/pve-manager\/([\d.]+)/.exec(h.pveVersion)?.[1] ?? h.pveVersion} />
            <Fact label="Kernel" value={h.kernel.replace(/^Linux /, '').split(' ')[0]} />
            <Fact label="ZFS" value={h.zfsVersion || '—'} />
            <Fact label="CPU" value={`${h.cpus} × ${h.cpuModel}`} />
            <Fact label="Memory" value={formatBytes(h.memoryBytes)} />
          </CardContent>
        </Card>
      )}
      <Tabs defaultValue="guests">
        <TabsList>
          <TabsTrigger value="guests">
            Guests ({inv.guests.length}
            {notReady > 0 && `, ${notReady} not ready`})
          </TabsTrigger>
          <TabsTrigger value="storage">Storage</TabsTrigger>
          <TabsTrigger value="network">Network</TabsTrigger>
          <TabsTrigger value="zrepl">zrepl</TabsTrigger>
        </TabsList>
        <TabsContent value="guests">
          <GuestsTable guests={inv.guests} plans={guestPlans} />
        </TabsContent>
        <TabsContent value="storage">
          <StorageView inv={inv} />
        </TabsContent>
        <TabsContent value="network">
          <NetworkTable inv={inv} />
        </TabsContent>
        <TabsContent value="zrepl">
          <ZreplView inv={inv} />
        </TabsContent>
      </Tabs>
    </>
  )
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-muted-foreground">{label}</div>
      <div className="font-medium break-words">{value}</div>
    </div>
  )
}

const readinessLabel: Record<Readiness, string> = {
  [Readiness.UNSPECIFIED]: 'unknown',
  [Readiness.REPLICABLE]: 'replicable',
  [Readiness.NOT_REPLICABLE]: 'not replicable',
  [Readiness.NOT_NEEDED]: 'not needed',
}

type GuestPlans = GetHostInventoryResponse['guestPlans']

function GuestsTable({ guests, plans }: { guests: Guest[]; plans: GuestPlans }) {
  if (guests.length === 0) return <p className="py-8 text-center text-muted-foreground">No guests on this host.</p>
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>ID</TableHead>
          <TableHead>Name</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Resources</TableHead>
          <TableHead>Disks</TableHead>
          <TableHead>Network</TableHead>
          <TableHead>Replication</TableHead>
          <TableHead>Plan</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {guests.map((g) => (
          <TableRow key={g.vmid} className="align-top">
            <TableCell className="font-mono text-xs">{g.vmid}</TableCell>
            <TableCell>
              <div className="font-medium">{g.name}</div>
              <div className="text-xs text-muted-foreground">
                {g.type === GuestType.VM ? 'VM' : 'Container'}
                {g.template && ' · template'}
                {g.tags.length > 0 && ` · ${g.tags.join(', ')}`}
              </div>
            </TableCell>
            <TableCell className="text-xs">
              {g.status}
              {g.lock && <div className="text-warning-foreground">locked: {g.lock}</div>}
            </TableCell>
            <TableCell className="text-xs whitespace-nowrap">
              {g.cores > 0 ? `${g.cores} vCPU` : 'all CPUs'} · {formatBytes(g.memoryBytes)}
            </TableCell>
            <TableCell className="text-xs">
              <ul className="grid gap-1">
                {g.disks.map((d) => (
                  <li key={d.key} className={d.readiness === Readiness.NOT_REPLICABLE ? 'text-destructive' : ''}>
                    <span className="font-mono">{d.key}</span> {d.storage ? `${d.storage}:${d.volume}` : d.volume}
                    {d.sizeBytes > 0n && ` (${formatBytes(d.sizeBytes)})`}
                    <span className="text-muted-foreground"> · {readinessLabel[d.readiness]}</span>
                    {d.reason && d.readiness === Readiness.NOT_REPLICABLE && <div>{d.reason}</div>}
                  </li>
                ))}
              </ul>
            </TableCell>
            <TableCell className="text-xs">
              {g.nics.map((n) => (
                <div key={n.key}>
                  <span className="font-mono">{n.key}</span> {n.bridge}
                  {n.vlanTag > 0 && ` · VLAN ${n.vlanTag}`}
                </div>
              ))}
            </TableCell>
            <TableCell>
              {g.ready ? <Badge variant="success">Ready</Badge> : <Badge variant="destructive">Not ready</Badge>}
              {g.readinessWarnings.map((w) => (
                <div key={w} className="mt-1 flex gap-1 text-xs text-warning-foreground">
                  <TriangleAlert className="mt-0.5 size-3 shrink-0" /> {w}
                </div>
              ))}
            </TableCell>
            <TableCell className="text-xs">
              {plans[g.vmid] ? (
                <Link to={`/plans/${plans[g.vmid].id}`} className="hover:underline">
                  {plans[g.vmid].name}
                </Link>
              ) : (
                <span className="text-muted-foreground">unprotected</span>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

function StorageView({ inv }: { inv: Inventory }) {
  return (
    <div className="grid gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Proxmox storage</CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Content</TableHead>
                <TableHead>Used</TableHead>
                <TableHead>Replication</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {inv.storages.map((s) => (
                <TableRow key={s.id}>
                  <TableCell className="font-medium">{s.id}</TableCell>
                  <TableCell className="text-xs">
                    {s.type}
                    {s.zfsPool && ` (${s.zfsPool})`}
                  </TableCell>
                  <TableCell className="text-xs">{s.content.join(', ')}</TableCell>
                  <TableCell className="text-xs">
                    {s.totalBytes > 0n ? `${formatBytes(s.usedBytes)} of ${formatBytes(s.totalBytes)}` : '—'}
                  </TableCell>
                  <TableCell>
                    {s.type === 'zfspool' ? <Badge variant="success">ZFS</Badge> : <Badge variant="neutral">Not supported</Badge>}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>ZFS</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Pool</TableHead>
                <TableHead>Health</TableHead>
                <TableHead>Allocated</TableHead>
                <TableHead>Fragmentation</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {inv.zfsPools.map((p) => (
                <TableRow key={p.name}>
                  <TableCell className="font-medium">{p.name}</TableCell>
                  <TableCell>
                    <Badge variant={p.health === 'ONLINE' ? 'success' : 'destructive'}>{p.health}</Badge>
                  </TableCell>
                  <TableCell className="text-xs">
                    {formatBytes(p.allocatedBytes)} of {formatBytes(p.sizeBytes)}
                  </TableCell>
                  <TableCell className="text-xs">{p.fragmentationPercent}%</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Dataset</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Used</TableHead>
                <TableHead>Compression</TableHead>
                <TableHead>Encryption</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {inv.zfsDatasets.map((d) => (
                <TableRow key={d.name}>
                  <TableCell className="font-mono text-xs">{d.name}</TableCell>
                  <TableCell className="text-xs">
                    {d.type === 'volume' ? `zvol (${formatBytes(d.volumeSizeBytes)})` : d.type}
                  </TableCell>
                  <TableCell className="text-xs">{formatBytes(d.usedBytes)}</TableCell>
                  <TableCell className="text-xs">{d.compression}</TableCell>
                  <TableCell className="text-xs">{d.encryption}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

function NetworkTable({ inv }: { inv: Inventory }) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Interface</TableHead>
          <TableHead>Type</TableHead>
          <TableHead>Details</TableHead>
          <TableHead>Address</TableHead>
          <TableHead>State</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {inv.interfaces.map((i) => (
          <TableRow key={i.name}>
            <TableCell className="font-mono text-xs">{i.name}</TableCell>
            <TableCell className="text-xs">{i.type}</TableCell>
            <TableCell className="text-xs">
              {i.type === 'bridge' &&
                `ports: ${i.bridgePorts.join(', ') || 'none'}${i.vlanAware ? ' · VLAN-aware' : ''}`}
              {i.type === 'vlan' && `VLAN ${i.vlanId} on ${i.vlanRawDevice}`}
              {i.type === 'bond' && `members: ${i.bondMembers.join(', ')}`}
            </TableCell>
            <TableCell className="font-mono text-xs">
              {i.cidr}
              {i.gateway && <div className="text-muted-foreground">gw {i.gateway}</div>}
            </TableCell>
            <TableCell className="text-xs">{i.active ? 'active' : 'inactive'}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

function ZreplView({ inv }: { inv: Inventory }) {
  const z = inv.zrepl
  if (!z) return <p className="text-sm text-muted-foreground">This host's client doesn't report zrepl details yet.</p>
  return (
    <div className="grid gap-4">
      <Card>
        <CardContent className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <Fact label="Version" value={z.version || 'not installed'} />
          <Fact label="Service" value={z.version ? (z.running ? 'running' : 'stopped') : '—'} />
          <Fact label="Jobs" value={`${z.jobs.length} (${z.jobs.filter((j) => j.managed).length} managed by EZDR)`} />
        </CardContent>
      </Card>
      {z.configError && (
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>Part of the zrepl configuration couldn't be read</AlertTitle>
          <AlertDescription className="font-mono text-xs break-all">{z.configError}</AlertDescription>
        </Alert>
      )}
      {z.jobs.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Job</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Network</TableHead>
              <TableHead>Details</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {z.jobs.map((j) => (
              <TableRow key={j.name} className="align-top">
                <TableCell>
                  <div className="font-mono text-xs">{j.name}</div>
                  <div className="mt-1">
                    {j.managed ? <Badge>EZDR</Badge> : <Badge variant="neutral">Hand-written</Badge>}
                  </div>
                  <div className="mt-1 text-xs text-muted-foreground break-all">{j.file}</div>
                </TableCell>
                <TableCell className="text-xs">{j.type}</TableCell>
                <TableCell className="text-xs">
                  <ZreplNetwork job={j} />
                </TableCell>
                <TableCell className="text-xs">
                  <ZreplDetails job={j} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  )
}

function ZreplNetwork({ job: j }: { job: ZreplJob }) {
  return (
    <div className="grid gap-1">
      {j.listenAddress && (
        <div>
          {j.transport} listen <span className="font-mono">{j.listenAddress}</span>
          {j.listenFreebind && ' (freebind)'}
        </div>
      )}
      {j.connectAddress && (
        <div>
          {j.transport} connect <span className="font-mono">{j.connectAddress}</span>
        </div>
      )}
      {j.clientCns.length > 0 && <div className="text-muted-foreground">clients: {j.clientCns.join(', ')}</div>}
      {j.serverCn && <div className="text-muted-foreground">server: {j.serverCn}</div>}
    </div>
  )
}

function ZreplDetails({ job: j }: { job: ZreplJob }) {
  const send = j.send
  const flags = send
    ? [send.raw && 'raw', send.encrypted && 'encrypted', send.compressed && 'compressed', send.largeBlocks && 'large blocks', send.embeddedData && 'embedded data'].filter(Boolean)
    : []
  return (
    <div className="grid gap-1">
      {j.snapshottingType && (
        <div>
          snapshots: {j.snapshottingType}
          {j.snapshotIntervalSeconds > 0 && ` every ${formatDuration(j.snapshotIntervalSeconds)}`}
          {j.snapshotPrefix && (
            <>
              , prefix <span className="font-mono">{j.snapshotPrefix}</span>
            </>
          )}
        </div>
      )}
      {j.filesystems.length > 0 && (
        <div>
          filesystems:{' '}
          {j.filesystems.map((f, i) => (
            <span key={f.pattern} className="font-mono">
              {i > 0 && ', '}
              {f.include ? '' : '−'}
              {f.pattern}
            </span>
          ))}
        </div>
      )}
      {j.rootFs && (
        <div>
          receive into <span className="font-mono">{j.rootFs}</span>
          {j.intervalSeconds > 0 && `, every ${formatDuration(j.intervalSeconds)}`}
        </div>
      )}
      {flags.length > 0 && <div>send: {flags.join(', ')}</div>}
      {j.keepSender.length > 0 && <div>keep on sender: {pruneSummary(j.keepSender)}</div>}
      {j.keepReceiver.length > 0 && <div>keep on receiver: {pruneSummary(j.keepReceiver)}</div>}
    </div>
  )
}

function pruneSummary(rules: ZreplPruneRule[]): string {
  return rules
    .map((r) => {
      if (r.type === 'grid') return `${r.grid}${r.regex ? ` (${r.regex})` : ''}`
      if (r.type === 'regex') return `${r.negate ? 'not ' : ''}${r.regex}`
      if (r.type === 'last_n') return `last ${r.count}`
      return r.type
    })
    .join('; ')
}
