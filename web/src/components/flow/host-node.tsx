import { Handle, type NodeProps, Position } from '@xyflow/react'
import { Box, FlaskConical, Lock, Monitor, Server } from 'lucide-react'
import { Link } from 'react-router'

import { type GuestCopy, type HostNode, type Section, size } from '@/components/flow/chart-model'
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
      <Link
        to={`/hosts/${host.id}`}
        className="flex items-center gap-2.5 rounded-t-lg border-b px-3 hover:bg-muted/50"
        style={{ height: size.header }}
      >
        <Server className="size-4 shrink-0 text-muted-foreground" />
        <div className="grid min-w-0 leading-tight">
          <span className="truncate font-medium">{host.hostname}</span>
          <span className="text-xs text-muted-foreground">
            {sideText[side]}
            {alsoOther && (side === 'primary' ? ' · also a DR host' : ' · also a primary')}
          </span>
        </div>
        <span
          className={`ml-auto flex items-center gap-1.5 text-xs ${host.online ? 'text-success' : 'text-destructive'}`}
          title={host.online ? 'Online' : `Offline, last seen ${formatRelative(host.lastSeenAt)}`}
        >
          <span className={`size-2 rounded-full bg-current ${host.online ? '' : 'animate-pulse'}`} />
          {host.online ? 'Online' : 'Offline'}
        </span>
      </Link>
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
  return (
    <div style={{ marginBottom: size.sectionGap }}>
      <div className="relative flex items-center gap-2 text-xs" style={{ height: size.sectionHeader }}>
        <Link to={`/plans/${plan.id}`} className="truncate font-medium hover:underline">
          {plan.name}
        </Link>
        {plan.testing && side === 'dr' && (
          <span className="flex items-center gap-1 rounded-full bg-info/12 px-1.5 py-0.5 text-[11px] text-info">
            <FlaskConical className="size-3" /> Test running
          </span>
        )}
        {side === 'primary' && (
          <Handle type="source" id={plan.id} position={Position.Right} isConnectable={false} className="opacity-0" style={{ right: -12 }} />
        )}
        {side === 'dr' && (
          <Handle type="target" id={plan.id} position={Position.Left} isConnectable={false} className="opacity-0" style={{ left: -12 }} />
        )}
      </div>
      {guests.length === 0 ? (
        <div className="flex items-center px-2 text-xs text-muted-foreground" style={{ height: size.guest }}>
          No guests
        </div>
      ) : (
        guests.map((g) => <GuestRow key={g.vmid} guest={g} to={`/plans/${plan.id}`} planName={plan.name} />)
      )}
    </div>
  )
}

function GuestRow({ guest, to, planName }: { guest: GuestCopy; to: string; planName?: string }) {
  const Icon = guest.vm ? Monitor : Box
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Link to={to} className="block" style={{ height: size.guest, paddingTop: 2, paddingBottom: 2 }}>
            <span
              className={`flex h-full items-center gap-2 rounded-md border-[1.5px] px-2 text-xs hover:brightness-95 dark:hover:brightness-125 ${roleStyle[guest.role]}`}
            >
              <Icon className="size-3.5 shrink-0 opacity-70" />
              <span className="font-mono text-[11px] opacity-70">{guest.vmid}</span>
              <span className="truncate">{guest.name}</span>
              {guest.locked && <Lock className="ml-auto size-3 shrink-0 opacity-70" />}
            </span>
          </Link>
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
  )
}
