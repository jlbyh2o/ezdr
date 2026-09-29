import { BaseEdge, EdgeLabelRenderer, type EdgeProps, getBezierPath } from '@xyflow/react'
import { Link } from 'react-router'

import { linkState, type PlanEdge, toneColor } from '@/components/flow/chart-model'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { formatBytes, formatDuration, formatRelative } from '@/lib/format'

const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)')

// PlanEdge connects a plan's section on the primary to its section on the
// DR host. While data moves, pulses travel along it in the direction of the
// copy.
export function PlanEdgeView({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, data }: EdgeProps<PlanEdge>) {
  const [path, labelX, labelY] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition })
  if (!data) return null
  const { plan } = data
  const st = linkState(plan)
  const color = toneColor[st.tone]
  const animate = st.flow && !reducedMotion.matches
  const t = plan.transfer
  return (
    <>
      {st.flow && (
        <path d={path} fill="none" stroke={color} strokeWidth={8} strokeOpacity={0.18} className="animate-pulse" />
      )}
      <BaseEdge
        id={id}
        path={path}
        style={{
          stroke: color,
          strokeWidth: st.flow ? 2.5 : 2,
          strokeDasharray: st.dashed ? '6 6' : st.flow && !animate ? '2 4' : undefined,
          opacity: st.tone === 'neutral' ? 0.6 : 0.9,
        }}
      />
      {animate &&
        [0, 1, 2].map((i) => (
          <circle key={i} r={4} fill={color}>
            <animateMotion
              dur="1.8s"
              begin={`-${i * 0.6}s`}
              repeatCount="indefinite"
              path={path}
              keyPoints={st.flow === 'reverse' ? '1;0' : '0;1'}
              keyTimes="0;1"
              calcMode="linear"
            />
          </circle>
        ))}
      <EdgeLabelRenderer>
        <div
          className="nodrag nopan absolute"
          style={{ transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`, pointerEvents: 'all' }}
        >
          <Tooltip>
            <TooltipTrigger
              render={
                <Link
                  to={`/plans/${plan.id}`}
                  className="flex max-w-56 flex-col items-center rounded-md border bg-card px-2.5 py-1 text-center shadow-sm hover:bg-muted"
                >
                  <span className="truncate text-xs font-medium">{plan.name}</span>
                  <span className="flex items-center gap-1 text-[11px] text-muted-foreground">
                    <span className="size-1.5 shrink-0 rounded-full" style={{ background: color }} />
                    <span className="truncate">{st.label}</span>
                  </span>
                </Link>
              }
            />
            <TooltipContent>
              <div className="grid gap-0.5">
                <span className="font-medium">{plan.name}</span>
                {plan.health?.message && <span>{plan.health.message}</span>}
                {plan.health?.lastReplicationAt && (
                  <span>Last replication {formatRelative(plan.health.lastReplicationAt)}</span>
                )}
                {!!plan.health?.rpoAlertSeconds && (
                  <span>Alert when older than {formatDuration(plan.health.rpoAlertSeconds)}</span>
                )}
                {t && (
                  <span>
                    {t.bytesExpected > 0n
                      ? `Copied ${formatBytes(t.bytesDone)} of ${formatBytes(t.bytesExpected)}`
                      : `Copied ${formatBytes(t.bytesDone)}`}
                    {t.startedAt ? `, started ${formatRelative(t.startedAt)}` : ''}
                  </span>
                )}
                {plan.pendingChanges && <span>Has changes not applied yet</span>}
              </div>
            </TooltipContent>
          </Tooltip>
        </div>
      </EdgeLabelRenderer>
    </>
  )
}
