import { MarkerType, type Edge, type Node } from "@xyflow/react"
import type { RuleThen } from "@/lib/api-types"
import type { TreeAnalysis } from "./decompile"
import type { Cond, Slot, TreeNode } from "./model"

/** Data every canvas node carries. One shape for all six node types keeps the components simple. */
export interface TreeData extends Record<string, unknown> {
  kind: "cond" | "leaf" | "placeholder" | "entry" | "rule" | "aggregate"
  cond?: Cond
  factLabel?: string
  semantics?: string
  then?: RuleThen
  ruleId?: string
  text?: string
  branch?: "yes" | "no"
  slotPath?: string
  parentId?: string
  editable?: boolean
  rtl?: boolean
  dim?: boolean
  hit?: boolean
  title?: string
}

export interface GraphOptions {
  rtl: boolean
  editable: boolean
  /** Rule ids that the last trial run matched; empty means "no trial yet, highlight nothing" */
  hitRuleIds: string[]
  yesLabel: string
  noLabel: string
  entryLabel: string
  aggregateLabel: string
  placeholderCond: string
  placeholderLeaf: string
}

export function buildGraph(
  analysis: TreeAnalysis,
  opts: GraphOptions
): { nodes: Node<TreeData>[]; edges: Edge[] } {
  const nodes: Node<TreeData>[] = []
  const edges: Edge[] = []
  const hit = new Set(opts.hitRuleIds)
  const highlighting = hit.size > 0

  const pushEdge = (source: string, target: string, label?: string, onPath = true) => {
    edges.push({
      id: `${source}->${target}`,
      source,
      target,
      label,
      labelStyle: { fontSize: 11, fill: "var(--muted-foreground)" },
      markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16 },
      style: highlighting && !onPath ? { opacity: 0.25 } : { strokeWidth: 1.6 },
    })
  }

  if (analysis.level === "L1") {
    const onPath = highlighting ? hitPathIds(analysis.root ?? null, hit) : null
    const dim = (id: string) => !!onPath && !onPath.has(id)

    const placeSlot = (
      slot: Slot,
      parentId: string | null,
      branch: "yes" | "no",
      path: string
    ) => {
      const link = (targetId: string) => {
        if (!parentId) return
        pushEdge(
          parentId,
          targetId,
          branch === "yes" ? opts.yesLabel : opts.noLabel,
          !dim(targetId)
        )
      }
      if (!slot) {
        const id = `p-${parentId}-${branch}`
        nodes.push({
          id,
          type: "placeholder",
          position: { x: 0, y: 0 },
          data: {
            kind: "placeholder",
            branch,
            parentId: parentId ?? undefined,
            slotPath: path,
            editable: opts.editable,
            rtl: opts.rtl,
            dim: dim(id),
            title: branch === "yes" ? opts.placeholderCond : opts.placeholderLeaf,
          },
        })
        link(id)
        return
      }
      if (slot.kind === "leaf") {
        nodes.push({
          id: slot.id,
          type: "leaf",
          position: { x: 0, y: 0 },
          data: {
            kind: "leaf",
            then: slot.then,
            ruleId: slot.originRuleId,
            rtl: opts.rtl,
            hit: !!slot.originRuleId && hit.has(slot.originRuleId),
            dim: dim(slot.id),
          },
        })
        link(slot.id)
        return
      }
      nodes.push({
        id: slot.id,
        type: "cond",
        position: { x: 0, y: 0 },
        data: {
          kind: "cond",
          cond: slot.cond,
          rtl: opts.rtl,
          editable: opts.editable,
          dim: dim(slot.id),
        },
      })
      link(slot.id)
      placeSlot(slot.yes, slot.id, "yes", `${path}y`)
      placeSlot(slot.no, slot.id, "no", `${path}n`)
    }

    const root = analysis.root ?? null
    if (!root) {
      nodes.push({
        id: "p-empty",
        type: "placeholder",
        position: { x: 0, y: 0 },
        data: {
          kind: "placeholder",
          branch: "yes",
          slotPath: "",
          editable: opts.editable,
          rtl: opts.rtl,
          title: opts.placeholderCond,
        },
      })
    } else {
      placeSlot(root, null, "yes", "")
    }
    return { nodes, edges }
  }

  if (analysis.level === "L2") {
    ;(analysis.paths ?? []).forEach((path, i) => {
      const isHit = hit.has(path.ruleId)
      path.conds.forEach((cond, j) => {
        const id = `p2-${i}-${j}`
        nodes.push({
          id,
          type: "cond",
          position: { x: 0, y: 0 },
          data: {
            kind: "cond",
            cond,
            rtl: opts.rtl,
            editable: false,
            hit: isHit,
            dim: highlighting && !isHit,
          },
        })
      })
      const leafId = `p2-${i}-leaf`
      const last = path.conds.length > 0 ? `p2-${i}-${path.conds.length - 1}` : null
      nodes.push({
        id: leafId,
        type: "leaf",
        position: { x: 0, y: 0 },
        data: { kind: "leaf", ruleId: path.ruleId, rtl: opts.rtl, hit: isHit, dim: highlighting && !isHit },
      })
      if (last) pushEdge(last, leafId, undefined, !highlighting || isHit)
    })
    return { nodes, edges }
  }

  // L3: entry -> one node per rule -> aggregate. Explicitly a picture of "several rules, one verdict".
  nodes.push({
    id: "l3-entry",
    type: "entry",
    position: { x: 0, y: 0 },
    data: { kind: "entry", text: opts.entryLabel, rtl: opts.rtl },
  })
  ;(analysis.flatRules ?? []).forEach((rule, i) => {
    const id = `l3-r-${i}`
    const isHit = hit.has(rule.ruleId)
    nodes.push({
      id,
      type: "rule",
      position: { x: 0, y: 0 },
      data: {
        kind: "rule",
        ruleId: rule.ruleId,
        text: rule.text,
        rtl: opts.rtl,
        hit: isHit,
        dim: highlighting && !isHit,
      },
    })
    pushEdge("l3-entry", id, undefined, !highlighting || isHit)
    pushEdge(id, "l3-aggregate", undefined, !highlighting || isHit)
  })
  nodes.push({
    id: "l3-aggregate",
    type: "aggregate",
    position: { x: 0, y: 0 },
    data: { kind: "aggregate", text: opts.aggregateLabel, rtl: opts.rtl },
  })
  return { nodes, edges }
}

/** Ids of tree nodes that lie on a path to a hit leaf; everything else is dimmed during a trial. */
function hitPathIds(root: TreeNode | null, hit: Set<string>): Set<string> {
  const onPath = new Set<string>()
  const visit = (node: TreeNode | null): boolean => {
    if (!node) return false
    if (node.kind === "leaf") {
      const isHit = !!node.originRuleId && hit.has(node.originRuleId)
      return isHit
    }
    const yes = visit(node.yes)
    const no = visit(node.no)
    if (yes || no) onPath.add(node.id)
    return yes || no
  }
  if (root) visit(root)
  return onPath
}
