import type { FactRecord } from "@/lib/api-types"

/**
 * One table cell, and one cross-table axis cell.
 *
 * The encoding is shared on purpose (`contracts/01` §3.2.4 / §3.2.5): a bare literal means equality, `{op,value}`
 * means a comparison, and `*` means "no constraint on this dimension". The cross table simply never allows `*`,
 * because an axis that constrains nothing turns the matrix back into a flat table.
 */
export const CELL_MODES = ["*", "eq", "ne", "gt", "gte", "lt", "lte"] as const
export type CellMode = (typeof CELL_MODES)[number]

export interface CellDraft {
  mode: CellMode
  value: string
}

export function cellToDraft(raw: unknown): CellDraft {
  if (raw === undefined || raw === null) return { mode: "*", value: "" }
  if (raw === "*") return { mode: "*", value: "" }
  if (raw && typeof raw === "object" && !Array.isArray(raw)) {
    const o = raw as { op?: string; value?: unknown }
    const mode = (CELL_MODES as readonly string[]).includes(o.op ?? "")
      ? (o.op as CellMode)
      : "eq"
    return { mode, value: stringify(o.value) }
  }
  return { mode: "eq", value: stringify(raw) }
}

export function draftToCell(draft: CellDraft, fact: FactRecord | undefined): unknown {
  if (draft.mode === "*") return "*"
  const value = literalValue(draft.value, fact)
  if (draft.mode === "eq") return value
  return { op: draft.mode, value }
}

export function literalValue(raw: string, fact: FactRecord | undefined): unknown {
  switch (fact?.type) {
    case "integer":
      return raw.trim() === "" ? 0 : Number(raw)
    case "decimal":
      // Decimal is carried as a string on purpose: Number("0.1") is not 0.1.
      return raw
    case "boolean":
      return raw === "true"
    default:
      return raw
  }
}

export function stringify(v: unknown): string {
  if (v === null || v === undefined) return ""
  if (typeof v === "object") return JSON.stringify(v)
  return String(v)
}

export function defaultText(fact: FactRecord | undefined): string {
  switch (fact?.type) {
    case "enum":
      return fact.enum?.[0] ?? ""
    case "boolean":
      return "true"
    case "integer":
      return "0"
    case "decimal":
      return "0"
    default:
      return ""
  }
}
