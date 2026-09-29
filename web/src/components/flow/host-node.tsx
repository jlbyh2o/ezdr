import { Handle, type NodeProps, Position, useUpdateNodeInternals } from '@xyflow/react'
import { Box, FlaskConical, Lock, Monitor, Server, Settings } from 'lucide-react'
import { useEffect } from 'react'
import { Link } from 'react-router'

import {
  type GuestCopy,
  guestHandle,
  type HostNode,
  linkState,
  type Section,
  sectionAllocated,
  size,
  storageDetails,
  toneColor,
} from '@/components/flow/chart-model'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { PlanState } from '@/gen/ezdr/portal/v1/portal_pb'
import { formatBytes, formatRelative } from '@/lib/format'

const roleStyle: Record<GuestCopy['role'], string> = {
  running: 'border-success/70 bg-success/5',
  standby: 'border-muted-foreground/35 bg-muted/60 text-muted-foreground',
  pending: 'border-dashed border-muted-foreground/35 text-muted-foreground',
  unconfigured: 'border-dashed border-warning/70 text-muted-foreground',
  unprotected: 'border-border text-muted-foreground opacity-60',
  test: 'border-dashed border-info/60 bg-info/5',
}

const roleText: Record<GuestCopy['role'], string> = {
  running: 'Running',
  standby: 'Standby',
  pending: 'Not replicated yet (draft plan)',
  unconfigured: 'Unconfigured: in no plan, and not marked unprotected',
  unprotected: 'Unprotected, by choice',
  test: 'Test failover copy, on the isolated test bridge',
}

const sideText = { primary: 'Primary', dr: 'DR host', none: 'Not in a plan' }

// HostNode draws a host with its guests grouped by plan. Heights come from
// size, so buildChart can lay nodes out before they render.
export function HostNodeView({ id, data }: NodeProps<HostNode>) {
  const { host, side, alsoOther, sections, tests, unconfigured, unprotected, configureAt } = data
  // The chart keeps each node's measured size across polls, so React Flow
  // only measures again when the size changes: tell it when the handles
  // change without that, such as a guest moving between plans.
  const updateNodeInternals = useUpdateNodeInternals()
  const handles = sections.map((s) => s.guests.map((g) => guestHandle(s.plan.id, g.vmid)).join()).join('|')
  useEffect(() => updateNodeInternals(id), [handles, id, updateNodeInternals])
  return (
    <div className="rounded-lg border bg-card text-card-foreground shadow-sm" style={{ width: size.width }}>
      <div className="flex items-center gap-2.5 border-b pr-2 pl-3" style={{ height: size.header }}>
        {/* The icon's color shows whether the host is connected. */}
        <Server
          className={`size-5 shrink-0 ${host.online ? 'text-success' : 'text-destructive'}`}
          role="img"
          aria-label={host.online ? 'Online' : 'Offline'}
        >
          <title>{host.online ? 'Online' : `Offline, last seen ${formatRelative(host.lastSeenAt)}`}</title>
        </Server>
        <div className="grid min-w-0 leading-tight">
          <Link to={`/hosts/${host.id}`} className="truncate font-medium hover:underline">
            {host.hostname}
          </Link>
          <span className="text-xs text-muted-foreground">
            {sideText[side]}
            {alsoOther && (side === 'primary' ? ' · also a DR host' : ' · also a primary')}
          </span>
        </div>
        <SettingsLink to={`/hosts/${host.id}`} label={`${host.hostname} settings`} className="ml-auto size-7" />
      </div>
      <div className="px-2.5" style={{ paddingTop: size.padding / 2, paddingBottom: size.padding / 2 }}>
        {sections.map((s) => (
          <PlanSection key={s.plan.id} section={s} side={side} />
        ))}
        {tests.map((t) => (
          <LooseGuests key={t.to} title={`Test failover · ${t.planName}`} guests={t.guests} to={t.to} />
        ))}
        <LooseGuests title="Unconfigured" guests={unconfigured} to={configureAt} />
        <LooseGuests title="Unprotected" guests={unprotected} to={configureAt} />
      </div>
    </div>
  )
}

// LooseGuests lists guests in no plan, all of them.
function LooseGuests({ title, guests, to }: { title: string; guests: GuestCopy[]; to: string }) {
  if (guests.length === 0) return null
  return (
    <div style={{ marginBottom: size.sectionGap }}>
      <div className="flex items-center text-xs text-muted-foreground" style={{ height: size.sectionHeader }}>
        {title} ({guests.length})
      </div>
      {guests.map((g) => (
        <GuestRow key={g.vmid} guest={g} to={to} />
      ))}
    </div>
  )
}

function PlanSection({ section, side }: { section: Section; side: 'primary' | 'dr' | 'none' }) {
  const { plan, guests } = section
  const st = linkState(plan)
  const total = sectionAllocated(section)
  // Replica sizes, once the portal reports the plan's guests.
  const replica = (vmid: number) =>
    plan.state === PlanState.DRAFT ? undefined : plan.guests.find((r) => r.vmid === vmid)?.replicaBytes
  return (
    <div style={{ marginBottom: size.sectionGap }}>
      <div className="flex items-center gap-2 text-xs" style={{ height: size.sectionHeader }}>
        <Link to={`/plans/${plan.id}`} className="shrink-0 font-medium hover:underline">
          {plan.name}
        </Link>
        {total > 0n && (
          <span className="shrink-0 text-[11px] text-muted-foreground tabular-nums" title="Allocated to the plan's guests">
            {formatBytes(total)}
          </span>
        )}
        {plan.testing && side === 'dr' && (
          <span className="flex shrink-0 items-center gap-1 rounded-full bg-info/12 px-1.5 py-0.5 text-[11px] text-info">
            <FlaskConical className="size-3" /> Test
          </span>
        )}
        <span className="ml-auto flex min-w-0 items-center gap-1 text-[11px] text-muted-foreground" title={plan.health?.message}>
          <span className="size-1.5 shrink-0 rounded-full" style={{ background: toneColor[st.tone] }} />
          <span className="truncate">{st.label}</span>
        </span>
      </div>
      {guests.length === 0 ? (
        <div className="flex items-center px-2 text-xs text-muted-foreground" style={{ height: size.guest }}>
          No guests
        </div>
      ) : (
        guests.map((g) => (
          <GuestRow
            key={g.vmid}
            guest={g}
            to={`/plans/${plan.id}#guests`}
            planName={plan.name}
            replica={replica(g.vmid)}
            handle={side === 'primary' || side === 'dr' ? { side, id: guestHandle(plan.id, g.vmid) } : undefined}
          />
        ))
      )}
    </div>
  )
}

function GuestRow({
  guest,
  to,
  planName,
  replica,
  handle,
}: {
  guest: GuestCopy
  // Where the guest is configured: its plan, or where to choose one.
  to: string
  planName?: string
  // The space its replicas use on the DR host, when known.
  replica?: bigint
  // Where the guest's edge attaches: the right side on the primary, the
  // left side on the DR host.
  handle?: { side: 'primary' | 'dr'; id: string }
}) {
  const Icon = guest.vm ? Monitor : Box
  const details = storageDetails(guest, replica)
  return (
    <div className="relative" style={{ height: size.guest, paddingTop: 2, paddingBottom: 2 }}>
      {handle?.side === 'primary' && (
        <Handle type="source" id={handle.id} position={Position.Right} isConnectable={false} className="opacity-0" style={{ right: -11 }} />
      )}
      {handle?.side === 'dr' && (
        <Handle type="target" id={handle.id} position={Position.Left} isConnectable={false} className="opacity-0" style={{ left: -11 }} />
      )}
      <div className={`flex h-full items-center gap-2 rounded-md border-[1.5px] pr-0.5 pl-2 text-xs ${roleStyle[guest.role]}`}>
        <Tooltip>
          <TooltipTrigger
            render={
              <span className="flex min-w-0 flex-1 items-center gap-2 self-stretch">
                <Icon className="size-3.5 shrink-0 opacity-70" />
                <span className="font-mono text-[11px] opacity-70">{guest.vmid}</span>
                <span className="truncate">{guest.name}</span>
                <span className="ml-auto flex shrink-0 items-center gap-1.5">
                  {guest.locked && <Lock className="size-3 opacity-70" />}
                  {guest.allocated > 0n && (
                    <span className="text-[11px] tabular-nums opacity-60">{formatBytes(guest.allocated)}</span>
                  )}
                </span>
              </span>
            }
          />
          <TooltipContent side="top">
            <div className="grid gap-0.5">
              <span className="font-medium">
                {guest.vmid} {guest.name} ({guest.vm ? 'VM' : 'container'})
              </span>
              <span>
                {roleText[guest.role]}
                {guest.status && guest.role !== 'pending' ? ` · ${guest.status}` : ''}
                {guest.locked ? ' · locked' : ''}
              </span>
              {planName && <span>Plan: {planName}</span>}
              {details.length > 0 && <span>Disks: {details.join(' · ')}</span>}
            </div>
          </TooltipContent>
        </Tooltip>
        <SettingsLink to={to} label={`${guest.name} settings`} className="size-5" />
      </div>
    </div>
  )
}

// SettingsLink is a gear that opens where something is configured.
function SettingsLink({ to, label, className }: { to: string; label: string; className?: string }) {
  return (
    <Link
      to={to}
      aria-label={label}
      title={label}
      className={`flex shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-foreground/10 hover:text-foreground ${className ?? ''}`}
    >
      <Settings className="size-3.5" />
    </Link>
  )
}
