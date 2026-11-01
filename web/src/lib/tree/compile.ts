import type { ExprNode, Rule, RuleThen } from "@/lib/api-types"
import { condToExpr, negate, type Cond, type LeafTreeNode, type Slot } from "./model"

/**
 * Tree -> `rules[]` (`决策树画布设计.md` §4).
 *
 * Each root-to-leaf path becomes one rule whose `when` is the conjunction of the path, and a "no"
 * branch contributes the complement of its condition. Paths are mutually exclusive (one takes `C`,
 * the other `¬C`), so whichever hit strategy the rule set uses, the conclusion is the same as the
 * one drawn on screen. That property is the whole reason this feature is allowed to exist.
 */

export interface CompileContext {
  scene: string
  /** Rule ids already present, so a minted id never collides with a preserved one. */
  usedIds?: Iterable<string>
  /** First priority handed to a *new* leaf; existing leaves keep the priority they came with. */
  nextPriority?: number
}

export function compileTree(root: Slot, ctx: CompileContext): Rule[] {
  const used = new Set(ctx.usedIds ?? [])
  let priority = ctx.nextPriority ?? 1000
  const out: Rule[] = []

  walk(root, [], (stack, leaf) => {
    const ruleId = leaf.originRuleId ?? mintId(stack, leaf.then, used)
    used.add(ruleId)
    out.push({
      ruleId,
      version: leaf.version ?? "v1",
      scene: ctx.scene,
      priority: leaf.priority ?? priority--,
      riskLevel: leaf.riskLevel ?? "L2",
      when: whenOf(stack),
      then: leaf.then,
    })
  })
  return out
}

function walk(node: Slot, stack: Cond[], emit: (stack: Cond[], leaf: LeafTreeNode) => void) {
  if (!node) return
  if (node.kind === "leaf") {
    emit(stack, node)
    return
  }
  walk(node.yes, [...stack, node.cond], emit)
  walk(node.no, [...stack, negate(node.cond)], emit)
}

function whenOf(stack: Cond[]): ExprNode {
  if (stack.length === 0) {
    // A leaf at the root means "always": keep it explicit instead of emitting an empty expression,
    // which the DSL rejects.
    return { type: "boolean", value: true }
  }
  if (stack.length === 1) return condToExpr(stack[0])
  return { op: "and", args: stack.map(condToExpr) }
}

/**
 * Stable rule id for a newly added path.
 *
 * Earlier drafts matched old rules by a content hash and renumbered whenever a condition changed.
 * That throws away the one thing the id is for: yesterday's decision log points at `R4001`, and an
 * audit trail that stops resolving because someone edited a threshold is worse than a slightly stale
 * id. So an existing leaf keeps its `originRuleId` through edits, and only leaves that did not exist
 * before get a minted id.
 */
function mintId(stack: Cond[], then: RuleThen, used: Set<string>): string {
  const body = JSON.stringify([
    stack.map((c) => [c.fact, c.op, c.value]),
    then.decision,
    then.reasonCode,
  ])
  const base = `R-${fnv1a(body)}`
  let id = base
  for (let n = 2; used.has(id); n++) id = `${base}-${n}`
  return id
}

/** 32-bit FNV-1a. Not a security hash - it only needs to be short, stable and collision-free enough. */
function fnv1a(s: string): string {
  let h = 0x811c9dc5
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i)
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return h.toString(16).padStart(8, "0")
}
