import { Handle, type NodeProps, Position } from '@xyflow/react'
import { Box, FlaskConical, Lock, Monitor, Server, Settings } from 'lucide-react'
import { Link } from 'react-router'

import { type GuestCopy, guestHandle, type HostNode, linkState, type Section, size, toneColor } from '@/components/flow/chart-model'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { formatRelative } from '@/lib/format'

const roleStyle: Record<GuestCopy['role'], string> = {
  running: 'border-success/70 bg-success/5',
  standby: 'border-muted-foreground/35 bg-muted/60 text-muted-foreground',
  pending: 'border-dashed border-muted-foreground/35 text-muted-foreground',
  unprotected: 'border-dashed border-border text-muted-foreground opacity-75',
}

const roleText: Record<GuestCopy['role'], string> = {
  running: 'Running',
  standby: 'Standby',
  pending: 'Not replicated yet (draft plan)',
  unprotected: 'Not protected by any plan',
}

const sideText = { primary: 'Primary', dr: 'DR host', none: 'Not in a plan' }

// HostNode draws a host with its guests grouped by plan. Heights come from
// size, so buildChart can lay nodes out before they render.
export function HostNodeView({ data }: NodeProps<HostNode>) {
  const { host, side, alsoOther, sections, unprotected } = data
  const hidden = unprotected.length - size.unprotectedMax
  const shown = hidden > 1 ? unprotected.slice(0, size.unprotectedMax) : unprotected
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
        {unprotected.length > 0 && (
          <div style={{ marginBottom: size.sectionGap }}>
            <div className="flex items-center text-xs text-muted-foreground" style={{ height: size.sectionHeader }}>
              Not protected ({unprotected.length})
            </div>
            {shown.map((g) => (
              <GuestRow key={g.vmid} guest={g} to={`/hosts/${host.id}`} />
            ))}
            {hidden > 1 && (
              <Link
                to={`/hosts/${host.id}`}
                className="flex items-center px-2 text-xs text-muted-foreground hover:text-foreground"
                style={{ height: size.guest }}
              >
                and {hidden} more
              </Link>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function PlanSection({ section, side }: { section: Section; side: 'primary' | 'dr' | 'none' }) {
  const { plan, guests } = section
  const st = linkState(plan)
  return (
    <div style={{ marginBottom: size.sectionGap }}>
      <div className="flex items-center gap-2 text-xs" style={{ height: size.sectionHeader }}>
        <Link to={`/plans/${plan.id}`} className="shrink-0 font-medium hover:underline">
          {plan.name}
        </Link>
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
  handle,
}: {
  guest: GuestCopy
  // Where the guest's settings are: its plan, or its host if unprotected.
  to: string
  planName?: string
  // Where the guest's edge attaches: the right side on the primary, the
  // left side on the DR host.
  handle?: { side: 'primary' | 'dr'; id: string }
}) {
  const Icon = guest.vm ? Monitor : Box
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
                {guest.locked && <Lock className="ml-auto size-3 shrink-0 opacity-70" />}
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
