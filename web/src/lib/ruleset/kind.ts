import type { RulesetDefinition } from "@/lib/api-types"

/**
 * How a rule set is **written**. Picked once at creation time and then fixed for the life of that
 * rule set, because the kinds expand into three different stored forms and the platform rejects a
 * definition that carries more than one.
 *
 * This is emphatically **not** a runtime concept: `contracts/01` §3.1 says a rule set is the unit of
 * modularity and expressions / decision tables are merely ways to write it. The tree has no stored
 * form at all - it compiles down to `rules[]`, which is why "tree" and "expression" share a storage
 * form while "table" and "cross" do not.
 */
export type AuthoringKind = "expression" | "table" | "tree" | "cross"

/**
 * What actually reaches the backend: `rules[]`, `table`, or `crossTable`.
 *
 * The third form was added for the cross table (`决策表设计.md` §2.1). It is a **storage** form, not a
 * runtime one: `ExpandCrossTable` turns every matrix cell into an ordinary rule before compilation, so
 * the IR and the engine stay exactly as they were.
 */
export type StorageForm = "expression" | "table" | "crossTable"

export type EditorView = "structured" | "expression" | "table" | "tree" | "cross"

/** A rule set is stored as a table/cross table exactly when it carries that block; everything else is `rules[]`. */
export function storageFormOf(
  def: Pick<RulesetDefinition, "table" | "crossTable" | "rules">
): StorageForm {
  if (def.crossTable) return "crossTable"
  return def.table ? "table" : "expression"
}

/**
 * Which views make sense for a stored form.
 *
 * Table form deliberately has no editable tree tab: the tree would have to be decompiled from the
 * rows expanded at compile time, and there would be no way to write an edit back into the table - the
 * two would silently drift apart. `决策树画布设计.md` §7 rules that out explicitly.
 *
 * Cross table form also has no `expression` tab. That tab renders `ruleset.rules`, which a matrix does
 * not have (its rules only exist in the dry-run IR), so it would be a permanently blank page -
 * indistinguishable from data loss. `决策表设计.md` §8.
 */
export function viewsFor(form: StorageForm): EditorView[] {
  if (form === "table") return ["table", "expression", "structured"]
  if (form === "crossTable") return ["cross", "structured"]
  return ["structured", "expression", "tree"]
}

/** Landing view right after creation, so the user lands where they chose to work. */
export function viewForKind(kind: AuthoringKind): EditorView {
  if (kind === "table") return "table"
  if (kind === "cross") return "cross"
  if (kind === "tree") return "tree"
  return "structured"
}

/** Parses `?view=` from the URL; anything unknown falls back to the storage form's first view. */
export function parseView(raw: string | null, form: StorageForm): EditorView {
  const allowed = viewsFor(form)
  return allowed.includes(raw as EditorView)
    ? (raw as EditorView)
    : allowed[0]
}
