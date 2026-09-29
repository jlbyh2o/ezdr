import { type GuestEdgeData, guestLink, toneColor } from '@/components/flow/chart-model'
import { PlanState } from '@/gen/ezdr/portal/v1/portal_pb'
import { formatBytes, formatDateTime, formatDuration, formatRelative } from '@/lib/format'

// GuestCard shows a guest's replication at a glance, next to the pointer
// while it's over the guest's line.
export function GuestCard({ data, x, y }: { data: GuestEdgeData; x: number; y: number }) {
  const { plan, guest, replication: r } = data
  const st = guestLink(plan, r)
  const back = plan.state === PlanState.FAILING_BACK
  // Keep the card on screen.
  const left = Math.min(x + 16, window.innerWidth - 340)
  const top = Math.min(y + 16, window.innerHeight - 220)
  const row = (label: string, value: React.ReactNode) => (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 truncate">{value}</dd>
    </>
  )
  return (
    <div
      className="pointer-events-none fixed z-50 w-80 rounded-lg border bg-popover p-3 text-xs text-popover-foreground shadow-lg"
      style={{ left, top }}
    >
      <div className="font-medium">
        {guest.vmid} {guest.name} <span className="font-normal text-muted-foreground">({guest.vm ? 'VM' : 'container'})</span>
      </div>
      <div className="mb-2 text-muted-foreground">
        {back ? `${data.drHostname} → ${data.primaryHostname}` : `${data.primaryHostname} → ${data.drHostname}`} · {plan.name}
      </div>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
        {row(
          'Status',
          <span className="flex items-center gap-1.5">
            <span className="size-1.5 shrink-0 rounded-full" style={{ background: toneColor[st.tone] }} />
            {st.label}
          </span>,
        )}
        {row(
          'Last replicated',
          r?.lastReplicatedAt ? (
            <span title={formatDateTime(r.lastReplicatedAt)}>{formatRelative(r.lastReplicatedAt)}</span>
          ) : plan.state === PlanState.ACTIVE ? (
            'not yet'
          ) : (
            '—'
          ),
        )}
        {r?.latestSnapshot && row('Snapshot', <span className="font-mono">{r.latestSnapshot}</span>)}
        {r?.transferring &&
          row(
            'Copying',
            r.bytesExpected > 0n
              ? `${Math.floor((Number(r.bytesDone) / Number(r.bytesExpected)) * 100)}% · ${formatBytes(r.bytesDone)} of ${formatBytes(r.bytesExpected)}`
              : formatBytes(r.bytesDone),
          )}
        {!!plan.health?.rpoAlertSeconds && row('Alert after', formatDuration(plan.health.rpoAlertSeconds))}
        {r && r.disks > 1 && row('Disks', r.disks)}
        {guest.allocated > 0n && row('Allocated', formatBytes(guest.allocated))}
        {guest.used > 0n && row('Used', `about ${formatBytes(guest.used)}${guest.usedZfsOnly ? ' (ZFS disks only)' : ''}`)}
        {r && plan.state !== PlanState.DRAFT && row('On DR host', r.replicaBytes > 0n ? `about ${formatBytes(r.replicaBytes)}` : 'not yet')}
        {r?.errors.map((e) => row('Error', <span className="whitespace-normal text-destructive">{e}</span>))}
      </dl>
    </div>
  )
}
