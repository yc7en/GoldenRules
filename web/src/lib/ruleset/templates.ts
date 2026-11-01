import type {
  ExprNode,
  FactRecord,
  Rule,
  RuleThen,
  RulesetDefinition,
  TypedLiteral,
} from "@/lib/api-types"
import type { AuthoringKind } from "./kind"

export interface BlankOptions {
  rulesetId: string
  scene: string
  name?: string
  facts: FactRecord[]
}

/**
 * Starter skeletons for the three authoring kinds.
 *
 * Every skeleton has to survive the backend dry-run on its own: `Analyze` rejects a rule set with no
 * rules, and rejects a table with no columns or no rows. "Create an empty rule set and fill it in
 * later" is therefore not an option - each kind needs one honest, working example the author edits
 * rather than an empty shell that fails validation the moment it is saved.
 */
export function blankDefinition(
  kind: AuthoringKind,
  opts: BlankOptions
): RulesetDefinition {
  const base: RulesetDefinition = {
    rulesetId: opts.rulesetId,
    ...(opts.name ? { name: opts.name } : {}),
    scene: opts.scene,
    version: "v1",
    hitStrategy: kind === "table" || kind === "tree" ? "FIRST" : "ALL",
    defaultDecision: "REJECT",
  }
  if (kind === "table") return { ...base, table: blankTable(opts.rulesetId, opts.facts) }
  return { ...base, rules: [starterRule(opts.scene, opts.facts)] }
}

/** The one condition every skeleton is built around: the most legible enum fact available. */
function starterCondition(facts: FactRecord[]): ExprNode {
  const fact =
    facts.find((f) => f.fact === "request.channel" && f.type === "enum") ??
    facts.find((f) => f.type === "enum" && (f.enum?.length ?? 0) > 0) ??
    facts[0]
  if (!fact) {
    // No catalog at all (the request failed): fall back to a constant-true condition, which is
    // valid on its own and still gives the author something they can see and replace.
    return { type: "boolean", value: true }
  }
  return {
    op: "eq",
    args: [{ fact: fact.fact }, literalFor(fact, fact.enum?.[0])],
  }
}

function literalFor(fact: FactRecord, sample?: string): TypedLiteral {
  switch (fact.type) {
    case "enum":
      return { type: "string", value: sample ?? fact.enum?.[0] ?? "" }
    case "boolean":
      return { type: "boolean", value: true }
    case "integer":
      return { type: "integer", value: 1 }
    case "decimal":
      return { type: "decimal", value: "1" }
    default:
      return { type: "string", value: sample ?? "" }
  }
}

function starterRule(scene: string, facts: FactRecord[]): Rule {
  return {
    ruleId: "R0001",
    version: "v1",
    scene,
    priority: 100,
    riskLevel: "L2",
    when: starterCondition(facts),
    then: { decision: "APPROVE", reasonCode: "OK" },
  }
}

function blankTable(id: string, facts: FactRecord[]) {
  const fact =
    facts.find((f) => f.fact === "request.channel" && f.type === "enum") ??
    facts.find((f) => f.type === "enum") ??
    facts[0]
  const column = fact
    ? { fact: fact.fact, type: fact.type === "enum" ? "enum" : fact.type }
    : { fact: "request.channel", type: "enum" }
  const then: RuleThen = { decision: "REVIEW", reasonCode: "DEFAULT_REVIEW" }
  return {
    tableId: `DT-${id}`.slice(0, 40),
    columns: [column],
    // One all-wildcard row: "every input lands here". A concrete value would look like a rule the
    // author meant, and would quietly approve traffic nobody asked about.
    rows: [{ conditions: { [column.fact]: "*" }, then }],
  }
}

/** Reason codes must start with a registered prefix or be exactly `OK`; otherwise dry-run warns. */
export const DECISION_VALUES = ["APPROVE", "REJECT", "REVIEW", "ROUTE"] as const
