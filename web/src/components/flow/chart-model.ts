import type { Edge, Node } from '@xyflow/react'

import { GuestType } from '@/gen/ezdr/inventory/v1/inventory_pb'
import {
  type GetOverviewResponse,
  HealthState,
  type OverviewGuest,
  type OverviewHost,
  type OverviewPlan,
  PlanState,
} from '@/gen/ezdr/portal/v1/portal_pb'
import { formatDuration } from '@/lib/format'
import { healthStates, type Tone } from '@/lib/status'

// The replication chart (docs/design/ui.md, section 3.1): primary hosts on
// the left, DR hosts on the right, each listing its guests by plan, with
// one edge per plan from the primary's section to the DR host's.

// A guest as shown in a host: running (green outline), standby (gray: a
// replica, or a stopped primary copy), pending (no replica yet), or
// unprotected (in no plan).
export type GuestRole = 'running' | 'standby' | 'pending' | 'unprotected'

export type GuestCopy = {
  vmid: number
  name: string
  vm: boolean
  role: GuestRole
  locked: boolean
  status: string
}

export type Section = { plan: OverviewPlan; guests: GuestCopy[] }

export type Side = 'primary' | 'dr' | 'none'

export type HostNodeData = {
  host: OverviewHost
  side: Side
  // Also plays the other role, in the other column.
  alsoOther: boolean
  sections: Section[]
  unprotected: GuestCopy[]
}

export type HostNode = Node<HostNodeData, 'host'>
export type PlanEdge = Edge<{ plan: OverviewPlan; vmid: number }, 'plan'>

// Sizes in pixels, shared with the host node's styles so the layout can
// compute each node's height.
export const size = {
  width: 320,
  columnGap: 280,
  rowGap: 24,
  header: 52,
  sectionHeader: 30,
  guest: 30,
  sectionGap: 8,
  padding: 10,
  unprotectedMax: 4,
}

const failoverStates = [PlanState.FAILING_OVER, PlanState.FAILED_OVER, PlanState.FAILING_BACK]

function copy(g: OverviewGuest | undefined, vmid: number, role: GuestRole, fallbackName = ''): GuestCopy {
  return {
    vmid,
    name: g?.name || fallbackName || `guest ${vmid}`,
    vm: g ? g.type === GuestType.VM : true,
    role,
    locked: !!g?.lock,
    status: g?.status ?? '',
  }
}

function running(g?: OverviewGuest): boolean {
  return g?.status === 'running' && !g.lock
}

export function guestHandle(planId: string, vmid: number): string {
  return `${planId}:${vmid}`
}

// nodeHeight matches how HostNode renders its data.
export function nodeHeight(d: HostNodeData): number {
  let h = size.header + size.padding
  for (const s of d.sections) h += size.sectionHeader + Math.max(1, s.guests.length) * size.guest + size.sectionGap
  if (d.unprotected.length > 0) {
    h += size.sectionHeader + Math.min(d.unprotected.length, size.unprotectedMax + 1) * size.guest + size.sectionGap
  }
  return h
}

export function buildChart(ov: GetOverviewResponse): { nodes: HostNode[]; edges: PlanEdge[]; height: number } {
  const hosts = new Map(ov.hosts.map((h) => [h.id, h]))
  const byName = (a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name)
  const plans = [...ov.plans].sort(byName)

  const primaryIds = [...new Set(plans.map((p) => p.primaryHostId))]
    .filter((id) => hosts.has(id))
    .sort((a, b) => hosts.get(a)!.hostname.localeCompare(hosts.get(b)!.hostname))
  // DR hosts in the order their primaries appear, to keep edges short.
  const drIds: string[] = []
  for (const pid of primaryIds) {
    for (const p of plans) {
      if (p.primaryHostId === pid && hosts.has(p.drHostId) && !drIds.includes(p.drHostId)) drIds.push(p.drHostId)
    }
  }

  const guestOf = (hostId: string, vmid: number) => hosts.get(hostId)?.guests.find((g) => g.vmid === vmid)

  const primaryNode = (id: string): HostNodeData => {
    const host = hosts.get(id)!
    const sections = plans
      .filter((p) => p.primaryHostId === id)
      .map((plan) => ({
        plan,
        guests: plan.vmids.map((vmid) => {
          const g = guestOf(id, vmid)
          return copy(g, vmid, running(g) ? 'running' : 'standby')
        }),
      }))
    const unprotected = host.guests
      .filter((g) => !g.planId && !g.template)
      .map((g) => copy(g, g.vmid, 'unprotected'))
    return { host, side: 'primary', alsoOther: drIds.includes(id), sections, unprotected }
  }

  const drNode = (id: string): HostNodeData => {
    const host = hosts.get(id)!
    const mine = plans
      .filter((p) => p.drHostId === id)
      .sort((a, b) => primaryIds.indexOf(a.primaryHostId) - primaryIds.indexOf(b.primaryHostId) || byName(a, b))
    const sections = mine.map((plan) => ({
      plan,
      guests: plan.vmids.map((vmid) => {
        const source = guestOf(plan.primaryHostId, vmid)
        const here = guestOf(id, vmid)
        let role: GuestRole = 'standby'
        if (plan.state === PlanState.DRAFT) role = 'pending'
        else if (failoverStates.includes(plan.state) && running(here)) role = 'running'
        // Named after the primary's guest; locked only if the copy here is.
        return { ...copy(source, vmid, role, here?.name), locked: !!here?.lock }
      }),
    }))
    // Guests registered for failed-over plans are shown in their plan.
    const registered = new Set(mine.filter((p) => failoverStates.includes(p.state)).flatMap((p) => p.vmids))
    const unprotected = host.guests
      .filter((g) => !g.planId && !g.template && !registered.has(g.vmid))
      .map((g) => copy(g, g.vmid, 'unprotected'))
    return { host, side: 'dr', alsoOther: primaryIds.includes(id), sections, unprotected }
  }

  const left = primaryIds.map(primaryNode)
  const right = drIds.map(drNode)
  const column = (items: HostNodeData[]) => items.reduce((sum, d) => sum + nodeHeight(d) + size.rowGap, -size.rowGap)
  const leftHeight = Math.max(0, column(left))
  const rightHeight = Math.max(0, column(right))
  const top = Math.max(leftHeight, rightHeight)

  const nodes: HostNode[] = []
  const place = (items: HostNodeData[], x: number, height: number) => {
    let y = (top - height) / 2
    for (const d of items) {
      nodes.push({ id: `${d.side}:${d.host.id}`, type: 'host', position: { x, y }, data: d, width: size.width })
      y += nodeHeight(d) + size.rowGap
    }
  }
  place(left, 0, leftHeight)
  place(right, size.width + size.columnGap, rightHeight)

  // Hosts in no plan go in a row below.
  const used = new Set([...primaryIds, ...drIds])
  const others = ov.hosts
    .filter((h) => !used.has(h.id))
    .sort((a, b) => a.hostname.localeCompare(b.hostname))
    .map(
      (host): HostNodeData => ({
        host,
        side: 'none',
        alsoOther: false,
        sections: [],
        unprotected: host.guests.filter((g) => !g.template).map((g) => copy(g, g.vmid, 'unprotected')),
      }),
    )
  let height = top
  if (others.length > 0) {
    const y = top + (top > 0 ? size.rowGap * 2 : 0)
    let rowHeight = 0
    others.forEach((d, i) => {
      nodes.push({
        id: `none:${d.host.id}`,
        type: 'host',
        position: { x: i * (size.width + size.rowGap), y },
        data: d,
        width: size.width,
      })
      rowHeight = Math.max(rowHeight, nodeHeight(d))
    })
    height = y + rowHeight
  }

  // One edge per guest, from its copy on the primary to its copy on the DR
  // host.
  const edges: PlanEdge[] = plans
    .filter((p) => primaryIds.includes(p.primaryHostId) && drIds.includes(p.drHostId))
    .flatMap((plan) =>
      plan.vmids.map((vmid) => ({
        id: `guest:${plan.id}:${vmid}`,
        type: 'plan' as const,
        source: `primary:${plan.primaryHostId}`,
        sourceHandle: guestHandle(plan.id, vmid),
        target: `dr:${plan.drHostId}`,
        targetHandle: guestHandle(plan.id, vmid),
        data: { plan, vmid },
      })),
    )
  return { nodes, edges, height }
}

// How a plan's edge looks: its color, label, and whether (and which way)
// data pulses along it.
export type LinkState = {
  tone: Tone
  label: string
  dashed: boolean
  flow: 'forward' | 'reverse' | null
}

export function linkState(plan: OverviewPlan): LinkState {
  switch (plan.state) {
    case PlanState.DRAFT:
      return { tone: 'neutral', label: 'Draft: not replicating', dashed: true, flow: null }
    case PlanState.PAUSED:
      return { tone: 'neutral', label: 'Paused', dashed: true, flow: null }
    case PlanState.FAILING_OVER:
      return { tone: 'info', label: 'Failing over', dashed: false, flow: 'forward' }
    case PlanState.FAILED_OVER:
      return { tone: 'warning', label: 'Failed over: running on the DR host', dashed: true, flow: null }
    case PlanState.FAILING_BACK:
      return { tone: 'info', label: 'Failing back', dashed: false, flow: plan.transfer ? 'reverse' : null }
  }
  const h = healthStates[plan.health?.state ?? HealthState.UNSPECIFIED]
  const rpo = plan.health?.rpoAgeSeconds ? `RPO ${formatDuration(plan.health.rpoAgeSeconds)}` : ''
  let label = [h.label, rpo].filter(Boolean).join(' · ')
  const t = plan.transfer
  if (t) {
    const pct = t.bytesExpected > 0n ? ` ${Math.floor((Number(t.bytesDone) / Number(t.bytesExpected)) * 100)}%` : ''
    label = `Replicating${pct} · ${rpo || h.label}`
  }
  return { tone: h.tone, label: label || 'Waiting for status', dashed: false, flow: t ? 'forward' : null }
}

// CSS colors for tones, for SVG strokes.
export const toneColor: Record<Tone, string> = {
  success: 'var(--success)',
  warning: 'var(--warning)',
  destructive: 'var(--destructive)',
  info: 'var(--info)',
  neutral: 'var(--muted-foreground)',
}
