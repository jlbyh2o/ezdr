import { BaseEdge, type EdgeProps, getBezierPath } from '@xyflow/react'

import { guestLink, type PlanEdge, toneColor } from '@/components/flow/chart-model'
import { PlanState } from '@/gen/ezdr/portal/v1/portal_pb'

const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)')

// PlanEdge connects a guest on the primary to its copy on the DR host, in
// its plan's color, with an arrow showing which way the plan copies data:
// toward the DR host, or back toward the primary while failing back. While
// the guest's data moves, pulses travel along it.
export function PlanEdgeView({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, data }: EdgeProps<PlanEdge>) {
  const [path] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition })
  if (!data) return null
  const st = guestLink(data.plan, data.replication)
  const color = toneColor[st.tone]
  const animate = st.flow && !reducedMotion.matches
  const back = data.plan.state === PlanState.FAILING_BACK
  const marker = `arrow-${id}`
  return (
    <>
      <defs>
        <marker
          id={marker}
          viewBox="0 0 10 10"
          refX="8"
          refY="5"
          markerWidth="9"
          markerHeight="9"
          orient="auto-start-reverse"
          markerUnits="userSpaceOnUse"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill={color} />
        </marker>
      </defs>
      {st.flow && (
        <path d={path} fill="none" stroke={color} strokeWidth={8} strokeOpacity={0.18} className="animate-pulse" />
      )}
      <BaseEdge
        id={id}
        path={path}
        className="ezdr-line"
        markerEnd={back ? undefined : `url(#${marker})`}
        markerStart={back ? `url(#${marker})` : undefined}
        interactionWidth={14}
        style={{
          stroke: color,
          strokeWidth: st.flow ? 2.5 : 1.75,
          strokeDasharray: st.dashed ? '6 6' : st.flow && !animate ? '2 4' : undefined,
          opacity: st.tone === 'neutral' ? 0.6 : 0.9,
        }}
      />
      {animate &&
        [0, 1].map((i) => (
          <circle key={i} r={4} fill={color} pointerEvents="none">
            <animateMotion
              dur="1.8s"
              begin={`-${i * 0.9}s`}
              repeatCount="indefinite"
              path={path}
              keyPoints={st.flow === 'reverse' ? '1;0' : '0;1'}
              keyTimes="0;1"
              calcMode="linear"
            />
          </circle>
        ))}
    </>
  )
}
