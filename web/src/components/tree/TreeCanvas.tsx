import { useMemo } from "react"
import { Background, Controls, ReactFlow, type Node } from "@xyflow/react"
import "@xyflow/react/dist/style.css"
import type { FactRecord } from "@/lib/api-types"
import type { TreeAnalysis } from "@/lib/tree/decompile"
import { buildGraph, type TreeData } from "@/lib/tree/graph"
import { layoutGraph } from "@/lib/tree/layout"
import { nodeTypes } from "./nodes"

export interface TreeCanvasLabels {
  yes: string
  no: string
  entry: string
  aggregate: string
  addCond: string
  addLeaf: string
  placeholderCond: string
  placeholderLeaf: string
}

export interface TreeCanvasProps {
  analysis: TreeAnalysis
  facts: FactRecord[]
  rtl: boolean
  editable: boolean
  hitRuleIds: string[]
  labels: TreeCanvasLabels
  selectedId: string | null
  onSelect: (id: string) => void
  onAdd: (parentId: string | null, branch: "yes" | "no", kind: "cond" | "leaf") => void
  /** Bumping this remounts the canvas, which is how "re-layout" re-runs fitView. */
  layoutKey: number
}

export function TreeCanvas({
  analysis,
  facts,
  rtl,
  editable,
  hitRuleIds,
  labels,
  selectedId,
  onSelect,
  onAdd,
  layoutKey,
}: TreeCanvasProps) {
  const factIndex = useMemo(() => {
    const m = new Map<string, FactRecord>()
    for (const f of facts) m.set(f.fact, f)
    return m
  }, [facts])

  const { nodes, edges } = useMemo(() => {
    const built = buildGraph(analysis, {
      rtl,
      editable,
      hitRuleIds,
      yesLabel: labels.yes,
      noLabel: labels.no,
      entryLabel: labels.entry,
      aggregateLabel: labels.aggregate,
      placeholderCond: labels.placeholderCond,
      placeholderLeaf: labels.placeholderLeaf,
    })
    const enriched = built.nodes.map((n) => {
      if (n.type === "cond") {
        const fact = n.data.cond ? factIndex.get(n.data.cond.fact) : undefined
        return {
          ...n,
          data: {
            ...n.data,
            factLabel: fact?.displayName ?? fact?.semantics ?? n.data.cond?.fact,
          },
        }
      }
      if (n.type === "placeholder") {
        return {
          ...n,
          data: {
            ...n.data,
            addCondLabel: labels.addCond,
            addLeafLabel: labels.addLeaf,
            // Handed to the node instead of read from a module-level store: the node is rendered by
            // React Flow far from here, and a callback in `data` keeps the editor's tree as the only
            // place that mutates structure.
            onAdd,
          },
        }
      }
      return n
    }) as Node<TreeData>[]
    return { nodes: layoutGraph(enriched, built.edges, rtl), edges: built.edges }
  }, [analysis, rtl, editable, hitRuleIds, labels, factIndex, onAdd])

  const selected = nodes.map((n) =>
    n.id === selectedId ? { ...n, selected: true } : n
  )

  return (
    <div className="h-[560px] w-full rounded-lg border bg-muted/10">
      <ReactFlow
        key={`${analysis.level}-${layoutKey}-${rtl ? "rtl" : "ltr"}`}
        nodes={selected}
        edges={edges}
        nodeTypes={nodeTypes}
        fitView
        fitViewOptions={{ padding: 0.18, maxZoom: 1 }}
        minZoom={0.25}
        maxZoom={1.75}
        // Positions come from layout only; dragging would imply that placement carries meaning.
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        onNodeClick={(_, node) => onSelect(node.id)}
        proOptions={{ hideAttribution: false }}
      >
        <Background gap={16} size={1} />
        {/* No MiniMap: a rule set has a handful of paths, and the minimap rendered as two grey
            blocks that read as a loading placeholder. The controls are still there for zooming. */}
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  )
}
