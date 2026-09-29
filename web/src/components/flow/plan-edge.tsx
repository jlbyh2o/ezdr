import { BaseEdge, type EdgeProps, getBezierPath } from '@xyflow/react'

import { linkState, type PlanEdge, toneColor } from '@/components/flow/chart-model'
import { formatBytes } from '@/lib/format'

const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)')

// PlanEdge connects a guest on the primary to its copy on the DR host, in
// its plan's color. While the plan's data moves, pulses travel along it in
// the direction of the copy.
export function PlanEdgeView({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, data }: EdgeProps<PlanEdge>) {
  const [path] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition })
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
        [0, 1].map((i) => (
          <circle key={i} r={4} fill={color}>
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
      {/* A wide invisible path, so hovering near the line shows what it is. */}
      <path d={path} fill="none" stroke="transparent" strokeWidth={12}>
        <title>
          {`${plan.name}, guest ${data.vmid}: ${st.label}`}
          {t ? ` (${t.bytesExpected > 0n ? `${formatBytes(t.bytesDone)} of ${formatBytes(t.bytesExpected)}` : formatBytes(t.bytesDone)})` : ''}
        </title>
      </path>
    </>
  )
}
