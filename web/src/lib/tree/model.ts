import type { ExprNode, RiskLevel, RuleThen } from "@/lib/api-types"

/**
 * Decision tree model.
 *
 * The tree is a **view over `rules[]`**, not a stored form (`决策树画布设计.md` §3): every root-to-leaf
 * path is the conjunction of the conditions on it, i.e. exactly one rule. That is the only reason a
 * canvas edit can be written back without touching the engine.
 */

export type LitValue = string | number | boolean | null | Array<string | number | boolean>

/** A typed literal, mirroring the shape the DSL uses for the right-hand side of a comparison. */
export interface Lit {
  type: string
  value: LitValue
}

/** A single condition: `fact op value`. Composite conditions never appear in the tree (§5 L3). */
export interface Cond {
  fact: string
  op: string
  value: Lit
}

/** A slot may be empty: that branch simply does not match and the input falls through to the default. */
export type Slot = TreeNode | null

export interface CondTreeNode {
  id: string
  kind: "cond"
  cond: Cond
  yes: Slot
  no: Slot
}

export interface LeafTreeNode {
  id: string
  kind: "leaf"
  then: RuleThen
  /** Rule this leaf came from - kept so a save does not renumber the rules behind yesterday's logs. */
  originRuleId?: string
  version?: string
  priority?: number
  riskLevel?: RiskLevel
}

export type TreeNode = CondTreeNode | LeafTreeNode

/** Operators a tree edge may carry. `and` / `or` / `not` are deliberately absent. */
export const COND_OPS = ["eq", "ne", "gt", "gte", "lt", "lte", "in", "notIn"] as const

/** Symmetric complements (`决策树画布设计.md` §4.2, verified against `Op::parse`). */
const NEGATION: Record<string, string> = {
  eq: "ne",
  ne: "eq",
  gt: "lte",
  lte: "gt",
  gte: "lt",
  lt: "gte",
  in: "notIn",
  notIn: "in",
}

/**
 * Which of a complementary pair becomes the "yes" branch. Reading "amount ≤ 1000 ? …" beats
 * "amount > 1000 ? …" for the same split, and `eq` before `ne` keeps the common case first.
 */
const POSITIVE_PREFERENCE = ["eq", "lte", "lt", "gte", "gt", "in", "ne", "notIn"]

export function negate(cond: Cond): Cond {
  const op = NEGATION[cond.op]
  if (!op) {
    // No symmetric complement exists (isNull / call / and / or): those never enter the tree, so this
    // is a programming error rather than a user-facing failure.
    throw new Error(`operator has no complement: ${cond.op}`)
  }
  return { ...cond, op }
}

export function sameCond(a: Cond, b: Cond): boolean {
  return (
    a.fact === b.fact &&
    a.op === b.op &&
    JSON.stringify(a.value) === JSON.stringify(b.value)
  )
}

export function areComplementary(a: Cond, b: Cond): boolean {
  return a.fact === b.fact && NEGATION[a.op] === b.op && sameCondLiteral(a.value, b.value)
}

function sameCondLiteral(a: Lit, b: Lit): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}

/** Chooses the readable side of a complementary pair. */
export function positiveOf(a: Cond, b: Cond): Cond {
  const ra = POSITIVE_PREFERENCE.indexOf(a.op)
  const rb = POSITIVE_PREFERENCE.indexOf(b.op)
  return ra <= rb ? a : b
}

// ---------------------------------------------------------------- expression <-> Cond

export function condToExpr(cond: Cond): ExprNode {
  return {
    op: cond.op,
    args: [{ fact: cond.fact }, { ...cond.value }],
  }
}

/**
 * Flattens `when` into a conjunction list. Returns null when the expression is not a pure conjunction
 * of comparisons (`or`, `not`, function calls, arithmetic on the right-hand side) - such rule sets
 * cannot be drawn as a tree at all.
 */
export function splitConjuncts(when: ExprNode): ExprNode[] | null {
  if (isOp(when, "and")) {
    const out: ExprNode[] = []
    for (const arg of when.args) {
      const inner = splitConjuncts(arg)
      if (!inner) return null
      out.push(...inner)
    }
    return out
  }
  return [when]
}

export function toCond(expr: ExprNode): Cond | null {
  if (!expr || typeof expr !== "object" || Array.isArray(expr)) return null
  const node = expr as { op?: unknown; args?: unknown }
  if (typeof node.op !== "string" || !COND_OPS.includes(node.op as never)) return null
  const args = node.args
  if (!Array.isArray(args) || args.length !== 2) return null
  const left = args[0]
  const right = args[1]
  if (!left || typeof left !== "object" || Array.isArray(left)) return null
  if (typeof (left as { fact?: unknown }).fact !== "string") return null
  const lit = asLit(right)
  if (!lit) return null
  return { fact: (left as { fact: string }).fact, op: node.op, value: lit }
}

/** Accepts the canonical `{type, value}` form only; a bare literal would be re-serialized differently. */
function asLit(v: ExprNode): Lit | null {
  if (!v || typeof v !== "object" || Array.isArray(v)) return null
  const obj = v as { type?: unknown; value?: unknown; fact?: unknown; op?: unknown }
  if (typeof obj.type !== "string" || obj.fact !== undefined || obj.op !== undefined) return null
  return { type: obj.type, value: obj.value as LitValue }
}

function isOp(v: ExprNode, op: string): v is { op: string; args: ExprNode[] } {
  return (
    !!v &&
    typeof v === "object" &&
    !Array.isArray(v) &&
    (v as { op?: unknown }).op === op &&
    Array.isArray((v as { args?: unknown }).args)
  )
}

/** Human-readable rendering for read-only views and node labels. Display only, never evaluated. */
export function condToText(cond: Cond): string {
  const sign: Record<string, string> = {
    eq: "=",
    ne: "≠",
    gt: ">",
    gte: "≥",
    lt: "<",
    lte: "≤",
    in: "∈",
    notIn: "∉",
  }
  const value = Array.isArray(cond.value.value)
    ? `{${cond.value.value.join(", ")}}`
    : String(cond.value.value)
  return `${cond.fact} ${sign[cond.op] ?? cond.op} ${value}`
}
