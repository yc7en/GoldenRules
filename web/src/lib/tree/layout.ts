import dagre from "@dagrejs/dagre"
import type { Edge, Node } from "@xyflow/react"

/**
 * Automatic layout with dagre. The tree's shape is defined by hierarchy alone - node positions carry
 * no meaning - so this runs on every structural change instead of letting anyone drag nodes around.
 */
const SIZE: Record<string, { w: number; h: number }> = {
  cond: { w: 248, h: 96 },
  leaf: { w: 232, h: 92 },
  placeholder: { w: 200, h: 52 },
  entry: { w: 168, h: 48 },
  rule: { w: 260, h: 64 },
  aggregate: { w: 200, h: 56 },
}

export function layoutGraph(
  nodes: Node[],
  edges: Edge[],
  rtl: boolean
): Node[] {
  const g = new dagre.graphlib.Graph()
  g.setGraph({
    // RTL mirrors the flow instead of mirroring the nodes: the tree still grows away from the root.
    rankdir: rtl ? "RL" : "LR",
    nodesep: 28,
    ranksep: 84,
    marginx: 24,
    marginy: 24,
  })
  g.setDefaultEdgeLabel(() => ({}))

  for (const n of nodes) {
    const size = SIZE[n.type ?? "cond"] ?? SIZE.cond
    g.setNode(n.id, { width: size.w, height: size.h })
  }
  for (const e of edges) g.setEdge(e.source, e.target)

  dagre.layout(g)

  return nodes.map((n) => {
    const p = g.node(n.id)
    const size = SIZE[n.type ?? "cond"] ?? SIZE.cond
    return {
      ...n,
      width: size.w,
      height: size.h,
      position: { x: (p?.x ?? 0) - size.w / 2, y: (p?.y ?? 0) - size.h / 2 },
    }
  })
}
