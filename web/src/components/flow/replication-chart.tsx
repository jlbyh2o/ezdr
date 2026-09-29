import '@xyflow/react/dist/base.css'

import { ReactFlow, ReactFlowProvider, useNodesInitialized, useReactFlow } from '@xyflow/react'
import { useEffect, useMemo, useRef } from 'react'

import { buildChart } from '@/components/flow/chart-model'
import { HostNodeView } from '@/components/flow/host-node'
import { PlanEdgeView } from '@/components/flow/plan-edge'
import type { GetOverviewResponse } from '@/gen/ezdr/portal/v1/portal_pb'

const nodeTypes = { host: HostNodeView }
const edgeTypes = { plan: PlanEdgeView }
const fitOptions = { padding: 0.04, maxZoom: 1 }
// React Flow only passes pointer events to nodes with a handler; the links
// inside the nodes do the navigating.
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
  const { nodes, edges, height } = useMemo(() => buildChart(overview), [overview])
  const { fitView } = useReactFlow()
  const initialized = useNodesInitialized()
  const box = useRef<HTMLDivElement>(null)

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
        panOnDrag={false}
        panOnScroll={false}
        zoomOnScroll={false}
        zoomOnPinch={false}
        zoomOnDoubleClick={false}
        preventScrolling={false}
        proOptions={{ hideAttribution: true }}
      />
    </div>
  )
}
