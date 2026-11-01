import type { FactRecord, RuleThen } from "@/lib/api-types"
import type { Cond, Lit, Slot, TreeNode } from "./model"

/**
 * Structural edits on the tree. All of them return a new tree instead of mutating: the canvas, the
 * compiled rules and the dirty flag are all derived from the same value, so an in-place mutation
 * would render a tree that the save button disagrees with.
 */

export function findNode(root: Slot, id: string): TreeNode | null {
  if (!root) return null
  if (root.id === id) return root
  if (root.kind !== "cond") return null
  return findNode(root.yes, id) ?? findNode(root.no, id)
}

export function updateCond(root: Slot, id: string, cond: Cond): Slot {
  if (!root) return root
  if (root.id === id) return root.kind === "cond" ? { ...root, cond } : root
  if (root.kind !== "cond") return root
  return {
    ...root,
    yes: updateCond(root.yes, id, cond),
    no: updateCond(root.no, id, cond),
  }
}

export function updateLeaf(root: Slot, id: string, then: RuleThen): Slot {
  if (!root) return root
  if (root.id === id) return root.kind === "leaf" ? { ...root, then } : root
  if (root.kind !== "cond") return root
  return {
    ...root,
    yes: updateLeaf(root.yes, id, then),
    no: updateLeaf(root.no, id, then),
  }
}

/** Fills a slot. `parentId` null targets the root itself (the "no tree yet" placeholder). */
export function setSlot(
  root: Slot,
  parentId: string | null,
  branch: "yes" | "no",
  slot: Slot
): Slot {
  if (!parentId) return slot
  if (!root) return root
  if (root.id === parentId && root.kind === "cond") {
    return { ...root, [branch]: slot }
  }
  if (root.kind !== "cond") return root
  return {
    ...root,
    yes: setSlot(root.yes, parentId, branch, slot),
    no: setSlot(root.no, parentId, branch, slot),
  }
}

export function removeNode(root: Slot, id: string): Slot {
  if (!root) return root
  if (root.id === id) return null
  if (root.kind !== "cond") return root
  return {
    ...root,
    yes: removeNode(root.yes, id),
    no: removeNode(root.no, id),
  }
}

let seq = 0
export function newCondNode(cond: Cond): TreeNode {
  seq += 1
  return {
    id: `c-new-${Date.now().toString(36)}-${seq}`,
    kind: "cond",
    cond,
    yes: null,
    no: null,
  }
}

export function newLeafNode(then: RuleThen): TreeNode {
  seq += 1
  return { id: `l-new-${Date.now().toString(36)}-${seq}`, kind: "leaf", then }
}

/** First condition offered when someone expands a placeholder: the catalog's first enum fact. */
export function defaultCond(facts: FactRecord[]): Cond {
  const fact = facts.find((f) => f.type === "enum" && (f.enum?.length ?? 0) > 0) ?? facts[0]
  if (!fact) return { fact: "request.channel", op: "eq", value: { type: "string", value: "" } }
  return { fact: fact.fact, op: "eq", value: literalFor(fact, fact.enum?.[0] ?? "") }
}

/** Only the operators that have a symmetric complement may be used as a tree condition (§4.2). */
export const TREE_OPS = ["eq", "ne", "gt", "gte", "lt", "lte", "in", "notIn"] as const

export function opsForFact(fact: FactRecord | undefined): string[] {
  if (!fact) return ["eq", "ne"]
  switch (fact.type) {
    case "decimal":
    case "integer":
      return ["eq", "ne", "gt", "gte", "lt", "lte", "in", "notIn"]
    case "boolean":
    case "enum":
    case "string":
      return ["eq", "ne", "in", "notIn"]
    default:
      return ["eq", "ne"]
  }
}

export function literalFor(fact: FactRecord, raw: string): Lit {
  switch (fact.type) {
    case "integer":
      return { type: "integer", value: raw === "" ? 0 : Number(raw) }
    case "decimal":
      // Decimal stays a string end to end: parsing it into a JS number would round money.
      return { type: "decimal", value: raw === "" ? "0" : raw }
    case "boolean":
      return { type: "boolean", value: raw === "true" }
    default:
      return { type: "string", value: raw }
  }
}

/** Text form of a literal, so a value can round-trip through an <input>. */
export function literalText(lit: Lit): string {
  if (Array.isArray(lit.value)) return lit.value.join(", ")
  if (lit.value === null) return ""
  return String(lit.value)
}

export function parseLiteralInput(
  fact: FactRecord | undefined,
  cond: Cond,
  raw: string
): Lit {
  if (cond.op === "in" || cond.op === "notIn") {
    const items = raw
      .split(",")
      .map((s) => s.trim())
      .filter((s) => s !== "")
    return {
      type: "array",
      value: items.map((s) => (fact ? parsedOne(fact, s) : s)),
    }
  }
  return fact ? literalFor(fact, raw) : { type: "string", value: raw }
}

function parsedOne(fact: FactRecord, raw: string): string | number | boolean {
  const lit = literalFor(fact, raw)
  return lit.value as string | number | boolean
}
