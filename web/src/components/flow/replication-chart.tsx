import '@xyflow/react/dist/base.css'

import { ReactFlow, ReactFlowProvider, useNodesInitialized, useReactFlow } from '@xyflow/react'
import { useEffect, useMemo, useRef, useState } from 'react'

import { buildChart, type PlanEdge } from '@/components/flow/chart-model'
import { GuestCard } from '@/components/flow/guest-card'
import { HostNodeView } from '@/components/flow/host-node'
import { PlanEdgeView } from '@/components/flow/plan-edge'
import type { GetOverviewResponse } from '@/gen/ezdr/portal/v1/portal_pb'

const nodeTypes = { host: HostNodeView }
const edgeTypes = { plan: PlanEdgeView }
const fitOptions = { padding: 0.04, maxZoom: 1 }
// React Flow only passes pointer events to nodes and edges with a click
// handler; links inside the nodes do the navigating, and edges show a card
// on hover.
const noop = () => undefined

// ReplicationChart shows where every guest replicates: primary hosts on the
// left, DR hosts on the right, one edge per plan. It isn't a canvas: no
// panning, zooming, or dragging; it scales down to fit narrow screens.
export function ReplicationChart({ overview }: { overview: GetOverviewResponse }) {
  return (
    <ReactFlowProvider>
      <Chart overview={overview} />
    </ReactFlowProvider>
  )
}

function Chart({ overview }: { overview: GetOverviewResponse }) {
  const { fitView, getInternalNode } = useReactFlow()
  // Each poll brings new node objects. Without the size React Flow measured,
  // it forgets where their handles are and every edge unmounts until it
  // measures again: each edge's <svg> is replaced, which restarts the pulses
  // moving along it. Nodes whose size changes are measured again anyway.
  const { nodes, edges, height } = useMemo(() => {
    const chart = buildChart(overview)
    for (const n of chart.nodes) {
      const m = getInternalNode(n.id)?.measured
      if (m?.width !== undefined && m.height !== undefined) n.measured = { width: m.width, height: m.height }
    }
    return chart
  }, [overview, getInternalNode])
  const initialized = useNodesInitialized()
  const box = useRef<HTMLDivElement>(null)
  const [hover, setHover] = useState<{ id: string; x: number; y: number }>()
  const hovered = hover && edges.find((e) => e.id === hover.id)
  const track = (event: React.MouseEvent, edge: PlanEdge) => setHover({ id: edge.id, x: event.clientX, y: event.clientY })

  // Refit when the layout or the available width changes.
  const layout = nodes.map((n) => `${n.id}@${n.position.y}`).join()
  useEffect(() => {
    if (initialized) void fitView(fitOptions)
  }, [initialized, layout, fitView])
  useEffect(() => {
    if (!box.current) return
    const observer = new ResizeObserver(() => void fitView(fitOptions))
    observer.observe(box.current)
    return () => observer.disconnect()
  }, [fitView])

  return (
    <div ref={box} className="w-full" style={{ height: height + 40 }}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        fitView
        fitViewOptions={fitOptions}
        nodesDraggable={false}
        nodesConnectable={false}
        nodesFocusable={false}
        edgesFocusable={false}
        elementsSelectable={false}
        onNodeClick={noop}
        onEdgeClick={noop}
        onEdgeMouseEnter={track}
        onEdgeMouseMove={track}
        onEdgeMouseLeave={() => setHover(undefined)}
        panOnDrag={false}
        panOnScroll={false}
        zoomOnScroll={false}
        zoomOnPinch={false}
        zoomOnDoubleClick={false}
        preventScrolling={false}
        proOptions={{ hideAttribution: true }}
      />
      {hover && hovered?.data && <GuestCard data={hovered.data} x={hover.x} y={hover.y} />}
    </div>
  )
}
