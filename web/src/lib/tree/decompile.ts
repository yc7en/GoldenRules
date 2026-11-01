import type { Rule, RulesetDefinition } from "@/lib/api-types"
import type { ExprNode } from "@/lib/api-types"
import {
  areComplementary,
  positiveOf,
  splitConjuncts,
  toCond,
  type Cond,
  type Slot,
} from "./model"
import { exprToText } from "./text"

/**
 * `rules[]` -> tree, with an honest three-level fallback (`决策树画布设计.md` §5).
 *
 * Only L1 is editable. L2 and L3 are read-only on purpose: if the rules are not a tree shape, drawing
 * something tree-like and letting people drag it around would promise an edit path that does not
 * exist. The caller shows the concrete reason instead of a generic "unsupported".
 */
export type TreeLevel = "L1" | "L2" | "L3"

/**
 * Why the view stopped short of an editable tree. Typed as literal keys rather than `string` so the
 * translation call type-checks: a typo would otherwise surface as a raw key in the UI.
 */
export type TreeReasonKey =
  | "tree.reason.tableForm"
  | "tree.reason.empty"
  | "tree.reason.strategy"
  | "tree.reason.expression"
  | "tree.reason.ambiguous"
  | "tree.reason.noSharedFact"
  | "tree.reason.tooManyValues"
  | "tree.reason.notComplementary"

/** The subset `build` can fail with - it never returns a storage-form reason. */
type BuildFailure = "ambiguous" | "noSharedFact" | "tooManyValues" | "notComplementary"

export interface PathView {
  ruleId: string
  conds: Cond[]
  /** Readable form of the rule's `when`, so read-only views can show what the rule actually says. */
  text: string
}

export interface TreeAnalysis {
  level: TreeLevel
  /** i18n key explaining why the view is read-only; absent for L1 */
  reasonKey?: TreeReasonKey
  reasonVars?: Record<string, string | number>
  /** L1 only */
  root?: Slot
  /** L2 only: one vertical chain per rule */
  paths?: PathView[]
  /** L2 and L3: the rules as written, for the read-only listings */
  flatRules?: PathView[]
  ruleCount: number
}

interface RuleConds {
  rule: Rule
  conds: Cond[]
}

export function analyzeRuleset(def: RulesetDefinition): TreeAnalysis {
  const rules = def.rules ?? []
  const ruleCount = rules.length
  const flat = rules.map((r) => ({
    ruleId: r.ruleId,
    text: exprToText(r.when),
    conds: [] as Cond[],
  }))

  if (def.table) {
    return { level: "L3", reasonKey: "tree.reason.tableForm", ruleCount, flatRules: flat }
  }
  if (ruleCount === 0) {
    return { level: "L3", reasonKey: "tree.reason.empty", ruleCount, flatRules: flat }
  }
  if ((def.hitStrategy ?? "ALL") !== "FIRST") {
    // Several rules may match at once and merge their outputs; a single path cannot express that.
    return {
      level: "L3",
      reasonKey: "tree.reason.strategy",
      reasonVars: { strategy: def.hitStrategy ?? "ALL" },
      ruleCount,
      flatRules: flat,
    }
  }

  const parsed: RuleConds[] = []
  for (const rule of rules) {
    const conds = condsOf(rule.when)
    if (!conds) {
      return {
        level: "L3",
        reasonKey: "tree.reason.expression",
        reasonVars: { ruleId: rule.ruleId },
        ruleCount,
        flatRules: flat,
      }
    }
    parsed.push({ rule, conds })
  }

  const built = build(parsed, "")
  if (!built.ok) {
    return {
      level: "L2",
      reasonKey: `tree.reason.${built.reason}`,
      reasonVars: built.vars,
      paths: parsed.map((p) => ({
        ruleId: p.rule.ruleId,
        conds: p.conds,
        text: exprToText(p.rule.when),
      })),
      flatRules: flat,
      ruleCount,
    }
  }
  return { level: "L1", root: built.root, ruleCount, flatRules: flat }
}

/** `null` means "not a conjunction of comparisons" - an `or`, a `not`, a function call, arithmetic. */
function condsOf(when: ExprNode): Cond[] | null {
  const parts = splitConjuncts(when)
  if (!parts) return null
  const out: Cond[] = []
  for (const part of parts) {
    const cond = toCond(part)
    if (!cond) return null
    out.push(cond)
  }
  return out
}

type BuildResult =
  | { ok: true; root: Slot }
  | { ok: false; reason: BuildFailure; vars?: Record<string, string | number> }

/**
 * Groups rules into a tree by repeatedly picking a fact that **every** remaining rule constrains.
 *
 * That "every rule" requirement is what makes the drawing truthful: a rule that does not constrain
 * the pivot would hold on both branches, so it would appear twice and the same input could take two
 * paths. When no such fact exists the rules are simply independent, and the caller falls back to L2.
 */
function build(rules: RuleConds[], path: string): BuildResult {
  if (rules.length === 0) return { ok: true, root: null }

  const empty = rules.filter((r) => r.conds.length === 0)
  if (empty.length > 0) {
    if (rules.length > 1) return { ok: false, reason: "ambiguous" }
    return { ok: true, root: leafNode(empty[0].rule, path) }
  }

  const common = commonFacts(rules)
  if (common.length === 0) return { ok: false, reason: "noSharedFact" }

  // Prefer a fact that actually splits (complementary pair) over one that everything satisfies:
  // the split is what makes the drawing readable, and using a no-op split first just wastes a level.
  let pivot = common.find((fact) => {
    const distinct = distinctConds(rules, fact)
    return distinct.length === 2 && areComplementary(distinct[0], distinct[1])
  })
  if (!pivot) {
    pivot = common.find((fact) => distinctConds(rules, fact).length === 1)
  }
  if (!pivot) {
    const tooMany = common.some((f) => distinctConds(rules, f).length > 2)
    return { ok: false, reason: tooMany ? "tooManyValues" : "notComplementary" }
  }

  const distinct = distinctConds(rules, pivot)
  const positive =
    distinct.length === 1 ? distinct[0] : positiveOf(distinct[0], distinct[1])

  const yesRules: RuleConds[] = []
  const noRules: RuleConds[] = []
  for (const r of rules) {
    const cond = r.conds.find((c) => c.fact === pivot)
    if (!cond) continue
    const rest = r.conds.filter((c) => c !== cond)
    if (matches(cond, positive)) yesRules.push({ rule: r.rule, conds: rest })
    else noRules.push({ rule: r.rule, conds: rest })
  }

  const yes = build(yesRules, `${path}y`)
  if (!yes.ok) return yes
  const no = build(noRules, `${path}n`)
  if (!no.ok) return no

  return {
    ok: true,
    root: {
      id: `c-${path || "root"}`,
      kind: "cond",
      cond: positive,
      yes: yes.root,
      no: no.root,
    },
  }
}

function commonFacts(rules: RuleConds[]): string[] {
  const first = rules[0].conds.map((c) => c.fact)
  return first.filter((fact) => rules.every((r) => r.conds.some((c) => c.fact === fact)))
}

function distinctConds(rules: RuleConds[], fact: string): Cond[] {
  const out: Cond[] = []
  for (const r of rules) {
    const cond = r.conds.find((c) => c.fact === fact)
    if (!cond) continue
    const dup = out.some(
      (c) => c.op === cond.op && JSON.stringify(c.value) === JSON.stringify(cond.value)
    )
    if (!dup) out.push(cond)
  }
  return out
}

function matches(a: Cond, b: Cond): boolean {
  return a.op === b.op && JSON.stringify(a.value) === JSON.stringify(b.value)
}

function leafNode(rule: Rule, path: string) {
  return {
    id: `l-${rule.ruleId}-${path}`,
    kind: "leaf" as const,
    then: rule.then,
    originRuleId: rule.ruleId,
    version: rule.version,
    priority: rule.priority,
    riskLevel: rule.riskLevel,
  }
}
