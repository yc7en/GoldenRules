import type { RulesetDefinition } from "@/lib/api-types"

/**
 * How a rule set is **written**. Picked once at creation time and then fixed for the life of that
 * rule set, because the three kinds expand into two different stored forms and the platform rejects
 * a definition that carries both.
 *
 * This is emphatically **not** a runtime concept: `contracts/01` §3.1 says a rule set is the unit of
 * modularity and expressions / decision tables are merely ways to write it. The tree has no stored
 * form at all - it compiles down to `rules[]`, which is why "tree" and "expression" share a storage
 * form while "table" does not.
 */
export type AuthoringKind = "expression" | "table" | "tree"

/** What actually reaches the backend: `rules[]` or `table`. There is no third option. */
export type StorageForm = "expression" | "table"

export type EditorView = "structured" | "expression" | "table" | "tree"

/** A rule set is stored as a table exactly when it has a `table` block; everything else is `rules[]`. */
export function storageFormOf(
  def: Pick<RulesetDefinition, "table" | "rules">
): StorageForm {
  return def.table ? "table" : "expression"
}

/**
 * Which views make sense for a stored form.
 *
 * Table form deliberately has no editable tree tab: the tree would have to be decompiled from the
 * rows expanded at compile time, and there would be no way to write an edit back into the table - the
 * two would silently drift apart. `决策树画布设计.md` §7 rules that out explicitly.
 */
export function viewsFor(form: StorageForm): EditorView[] {
  return form === "table"
    ? ["table", "expression", "structured"]
    : ["structured", "expression", "tree"]
}

/** Landing view right after creation, so the user lands where they chose to work. */
export function viewForKind(kind: AuthoringKind): EditorView {
  return kind === "table" ? "table" : kind === "tree" ? "tree" : "structured"
}

/** Parses `?view=` from the URL; anything unknown falls back to the storage form's first view. */
export function parseView(raw: string | null, form: StorageForm): EditorView {
  const allowed = viewsFor(form)
  return allowed.includes(raw as EditorView)
    ? (raw as EditorView)
    : allowed[0]
}
