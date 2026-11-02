import { useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { Plus, Trash2 } from "lucide-react"
import type { CrossHeader, CrossTableDef, FactRecord, RuleThen, TypedLiteral } from "@/lib/api-types"
import {
  CELL_MODES,
  cellToDraft,
  defaultText,
  draftToCell,
  stringify,
  type CellMode,
} from "@/lib/ruleset/cell"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { cn } from "@/lib/utils"

/**
 * Cross decision table editor (decision matrix, `contracts/01` §3.2.5).
 *
 * Two axes of conditions pick out exactly one cell, and that cell's literal is assigned to one target variable.
 * The whole table shares a single conclusion - the point of a matrix is a uniform decision with a varying
 * parameter (fee tables, rate tiers). When cells need different conclusions, this is the wrong form and the
 * author should have picked a plain decision table; the two forms are never converted into each other.
 *
 * Axis cells reuse the plain table's cell encoding, minus the wildcard: an axis that constrains nothing would
 * make the two-dimensional promise a lie, and the backend rejects it for the same reason.
 *
 * The layout is a real grid - one bordered cell per matrix square, row headers pinned to the start edge - because
 * the shape *is* the content here. A stack of "column 1 condition" inputs would make the reader reconstruct the
 * matrix in their head, which is the one thing this form exists to avoid.
 */

const DECISIONS = ["APPROVE", "REJECT", "REVIEW", "ROUTE"]
const TARGET_TYPES = ["string", "enum", "integer", "decimal", "boolean"] as const
const AXIS_MODES = CELL_MODES.filter((m) => m !== "*") as [CellMode, ...CellMode[]]

/** Grid lines are the layout: every cell draws its own, so `border-collapse` never leaves a gap. */
const LINE = "border border-border"
const HEAD = cn(LINE, "bg-muted/50 p-1 align-bottom text-start text-[10px] font-medium text-muted-foreground")
/** Pinned so that a wide matrix can be scrolled without losing which row a value belongs to. */
const PIN = "sticky start-0 z-10"
/** A cell-sized editor: no box of its own, the cell border is the box. */
const BARE =
  "w-full border-0 bg-transparent px-1 font-mono text-[11px] outline-none focus:ring-1 focus:ring-inset focus:ring-ring"

export interface CrossTableEditorProps {
  table: CrossTableDef
  facts: FactRecord[]
  onChange: (table: CrossTableDef) => void
  readOnlyReason?: string
}

export function CrossTableEditor({ table, facts, onChange, readOnlyReason }: CrossTableEditorProps) {
  const { t } = useTranslation()
  const readOnly = !!readOnlyReason
  const factIndex = new Map(facts.map((f) => [f.fact, f]))
  const rows = table.rowCells.length
  const cols = table.colCells.length

  const patch = (next: Partial<CrossTableDef>) => onChange({ ...table, ...next })

  // ------------------------------------------------------------ headers

  const setHeader = (axis: "row" | "col", index: number, fact: string) => {
    const key = axis === "row" ? "rowHeaders" : "colHeaders"
    const cellKey = axis === "row" ? "rowCells" : "colCells"
    const headers = table[key]
    const prev = headers[index]?.fact
    const prevFact = prev ? factIndex.get(prev) : undefined
    const nextFact = factIndex.get(fact)
    const nextHeaders = headers.map((h, i) => (i === index ? { fact, type: nextFact?.type } : h))
    // Renaming a dimension moves its condition values instead of leaving orphan keys behind: an undeclared
    // key in a header cell is a hard validation error, so a stale one would make the whole table invalid.
    // The value itself only survives the move when it still fits the new fact - carrying `"APP"` into an
    // integer dimension (or into a different enum) produces a cell that no longer compiles, and the author
    // would only find out from a type-mismatch refusal at save time. Resampled values skip whatever the other
    // lines already took, otherwise a rename could leave two identical lines behind.
    const taken: unknown[] = []
    const nextCells = table[cellKey].map((line) => {
      if (!prev || prev === fact) {
        taken.push(line[fact])
        return line
      }
      const copy = { ...line }
      copy[fact] = fitsValue(prevFact, nextFact, copy[prev]) ? copy[prev] : nextSample(nextFact, taken)
      delete copy[prev]
      taken.push(copy[fact])
      return copy
    })
    patch({ [key]: nextHeaders, [cellKey]: nextCells } as Partial<CrossTableDef>)
  }

  const addHeader = (axis: "row" | "col") => {
    const key = axis === "row" ? "rowHeaders" : "colHeaders"
    const used = new Set(table[key].map((h) => h.fact))
    const next = facts.find((f) => !used.has(f.fact))
    if (!next) return
    const headers = [...table[key], { fact: next.fact, type: next.type }]
    const cellKey = axis === "row" ? "rowCells" : "colCells"
    const cells = table[cellKey].map((line) => ({ ...line, [next.fact]: sampleFor(next) }))
    patch({ [key]: headers, [cellKey]: cells } as Partial<CrossTableDef>)
  }

  const removeHeader = (axis: "row" | "col", index: number) => {
    const key = axis === "row" ? "rowHeaders" : "colHeaders"
    const cellKey = axis === "row" ? "rowCells" : "colCells"
    if (table[key].length <= 1) return
    const dropped = table[key][index]?.fact
    const cells = table[cellKey].map((line) => {
      const copy = { ...line }
      if (dropped) delete copy[dropped]
      return copy
    })
    patch({
      [key]: table[key].filter((_, i) => i !== index),
      [cellKey]: cells,
    } as Partial<CrossTableDef>)
  }

  // ------------------------------------------------------------ rows / columns

  // A new line must not copy an existing condition: two lines with the same condition set make one of them
  // unreachable and the platform rejects the save. Pick the next unused value from the fact's own domain.
  const addRow = () => {
    const line: Record<string, unknown> = {}
    for (const h of table.rowHeaders) {
      line[h.fact] = nextSample(factIndex.get(h.fact), table.rowCells.map((l) => l[h.fact]))
    }
    patch({
      rowCells: [...table.rowCells, line],
      cells: [...table.cells, table.colCells.map(() => null)],
    })
  }

  const addCol = () => {
    const line: Record<string, unknown> = {}
    for (const h of table.colHeaders) {
      line[h.fact] = nextSample(factIndex.get(h.fact), table.colCells.map((l) => l[h.fact]))
    }
    patch({
      colCells: [...table.colCells, line],
      cells: table.cells.map((row) => [...row, null]),
    })
  }

  const removeRow = (i: number) => {
    if (rows <= 1) return
    patch({
      rowCells: table.rowCells.filter((_, x) => x !== i),
      cells: table.cells.filter((_, x) => x !== i),
    })
  }

  const removeCol = (j: number) => {
    if (cols <= 1) return
    patch({
      colCells: table.colCells.filter((_, x) => x !== j),
      cells: table.cells.map((row) => row.filter((_, x) => x !== j)),
    })
  }

  const setAxisCell = (axis: "row" | "col", line: number, fact: string, raw: unknown) => {
    const cellKey = axis === "row" ? "rowCells" : "colCells"
    patch({
      [cellKey]: table[cellKey].map((l, i) => (i === line ? { ...l, [fact]: raw } : l)),
    } as Partial<CrossTableDef>)
  }

  const setCell = (i: number, j: number, value: TypedLiteral | null) => {
    patch({
      cells: table.cells.map((row, ri) =>
        ri === i ? row.map((cell, cj) => (cj === j ? value : cell)) : row
      ),
    })
  }

  const filled = table.cells.flat().filter((c) => c !== null && c !== undefined).length
  // A boolean axis has exactly two rows; adding a third would duplicate one of them, and the platform would
  // reject the save. Offer the button only while the axis still has an unused value.
  const canAddRow = hasFreeLine(table.rowHeaders, table.rowCells, factIndex)
  const canAddCol = hasFreeLine(table.colHeaders, table.colCells, factIndex)

  return (
    <Card>
      <CardHeader className="py-2">
        <CardTitle className="flex flex-wrap items-center justify-between gap-2 text-sm">
          <span>
            {t("cross.title", { crossTableId: table.crossTableId })}
            <span className="ms-2 text-[11px] font-normal text-muted-foreground">
              {t("cross.shape", { rows, cols, filled })}
            </span>
          </span>
          {!readOnly && (
            <span className="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={addRow}
                disabled={!canAddRow}
                title={canAddRow ? undefined : t("cross.noFreeValue")}
              >
                <Plus className="size-3.5" />
                {t("cross.addRow")}
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={addCol}
                disabled={!canAddCol}
                title={canAddCol ? undefined : t("cross.noFreeValue")}
              >
                <Plus className="size-3.5" />
                {t("cross.addCol")}
              </Button>
            </span>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {readOnlyReason && (
          <p className="rounded-md border border-amber-300 bg-amber-50 px-2 py-1 text-[11px] text-amber-800">
            {readOnlyReason}
          </p>
        )}

        <div className="overflow-x-auto">
          <table className="w-full min-w-[620px] border-collapse text-xs">
            <thead>
              {/* Band 1 names the two axes. The pickers live here and only here - repeating them per column
                  would suggest each column has its own dimensions. */}
              <tr>
                <th rowSpan={2} className={cn(HEAD, PIN, "w-[210px] min-w-[190px] bg-muted")}>
                  <div className="text-[10px] font-medium text-muted-foreground">{t("cross.rowHeaders")}</div>
                  <HeaderPickers
                    axis="row"
                    headers={table.rowHeaders}
                    facts={facts}
                    readOnly={readOnly}
                    onPick={(i, fact) => setHeader("row", i, fact)}
                    onAdd={() => addHeader("row")}
                    onRemove={(i) => removeHeader("row", i)}
                  />
                </th>
                <th colSpan={cols} className={HEAD}>
                  <div className="text-[10px] font-medium text-muted-foreground">{t("cross.colHeaders")}</div>
                  <HeaderPickers
                    axis="col"
                    headers={table.colHeaders}
                    facts={facts}
                    readOnly={readOnly}
                    onPick={(i, fact) => setHeader("col", i, fact)}
                    onAdd={() => addHeader("col")}
                    onRemove={(i) => removeHeader("col", i)}
                  />
                </th>
              </tr>
              {/* Band 2 is the column axis: one condition set per column, in the same order as the pickers
                  above, so line k of every column belongs to dimension k. */}
              <tr>
                {table.colCells.map((line, j) => (
                  <th key={j} className={cn(LINE, "relative min-w-[128px] bg-muted/20 p-1 align-top")}>
                    {!readOnly && cols > 1 && (
                      <button
                        type="button"
                        className="absolute end-0.5 top-0.5 rounded p-0.5 text-muted-foreground/60 hover:text-red-600"
                        onClick={() => removeCol(j)}
                        aria-label={t("cross.removeCol")}
                      >
                        <Trash2 className="size-2.5" />
                      </button>
                    )}
                    <div className="space-y-0.5 pe-2.5">
                      {table.colHeaders.map((h) => (
                        <AxisCell
                          key={h.fact}
                          fact={factIndex.get(h.fact)}
                          raw={line[h.fact]}
                          readOnly={readOnly}
                          onChange={(raw) => setAxisCell("col", j, h.fact, raw)}
                        />
                      ))}
                    </div>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {table.rowCells.map((line, i) => (
                <tr key={i} className="group/row">
                  <th className={cn(HEAD, PIN, "bg-muted align-middle")}>
                    <div className="flex items-start gap-1">
                      <span className="w-3.5 shrink-0 pt-[3px] text-end text-[10px] font-normal tabular-nums text-muted-foreground">
                        {i + 1}
                      </span>
                      <div className="flex-1 space-y-0.5">
                        {table.rowHeaders.map((h) => (
                          <AxisCell
                            key={h.fact}
                            fact={factIndex.get(h.fact)}
                            raw={line[h.fact]}
                            readOnly={readOnly}
                            onChange={(raw) => setAxisCell("row", i, h.fact, raw)}
                          />
                        ))}
                      </div>
                      {!readOnly && rows > 1 && (
                        <button
                          type="button"
                          className="rounded p-0.5 text-muted-foreground opacity-0 hover:text-red-600 group-hover/row:opacity-100 focus-visible:opacity-100"
                          onClick={() => removeRow(i)}
                          aria-label={t("cross.removeRow")}
                        >
                          <Trash2 className="size-3" />
                        </button>
                      )}
                    </div>
                  </th>
                  {table.colCells.map((_, j) => (
                    <td key={j} className={cn(LINE, "group/cell relative p-0 align-middle")}>
                      <MatrixCell
                        value={table.cells[i]?.[j] ?? null}
                        targetType={table.target.type}
                        readOnly={readOnly}
                        onChange={(v) => setCell(i, j, v)}
                      />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
            {/* The table-level half of `then` sits in the grid's own footer rows: it applies to every cell
                above, and a footer reads that way without a label explaining it. */}
            <tfoot>
              <tr>
                <th className={cn(HEAD, PIN, "bg-muted align-middle")}>{t("cross.target")}</th>
                <td colSpan={cols} className={cn(LINE, "p-1")}>
                  {readOnly ? (
                    <span className="font-mono text-[11px]" dir="ltr">
                      {table.target.var} : {table.target.type}
                    </span>
                  ) : (
                    <span className="flex flex-wrap items-center gap-1" dir="ltr">
                      <input
                        className="h-7 w-40 rounded-sm border border-input bg-transparent px-1 font-mono text-[11px]"
                        value={table.target.var}
                        aria-label={t("cross.targetVar")}
                        onChange={(e) => patch({ target: { ...table.target, var: e.target.value } })}
                      />
                      <select
                        className="h-7 rounded-sm border border-input bg-transparent px-1 text-[11px]"
                        value={table.target.type}
                        aria-label={t("cross.targetType")}
                        onChange={(e) => patch({ target: { ...table.target, type: e.target.value } })}
                      >
                        {TARGET_TYPES.map((x) => (
                          <option key={x} value={x}>
                            {x}
                          </option>
                        ))}
                      </select>
                    </span>
                  )}
                </td>
              </tr>
              <tr>
                <th className={cn(HEAD, PIN, "bg-muted align-middle")}>{t("cross.then")}</th>
                <td colSpan={cols} className={cn(LINE, "p-1")}>
                  {readOnly ? (
                    <span className="font-mono text-[11px]" dir="ltr">
                      {table.then.decision} / {table.then.reasonCode}
                    </span>
                  ) : (
                    <span className="flex flex-wrap items-center gap-1" dir="ltr">
                      <select
                        className="h-7 rounded-sm border border-input bg-transparent px-1 text-[11px]"
                        value={table.then.decision}
                        onChange={(e) => patch({ then: { ...table.then, decision: e.target.value } })}
                      >
                        {DECISIONS.map((d) => (
                          <option key={d} value={d}>
                            {d}
                          </option>
                        ))}
                      </select>
                      <input
                        className="h-7 w-44 rounded-sm border border-input bg-transparent px-1 font-mono text-[11px]"
                        value={table.then.reasonCode}
                        aria-label={t("table.reasonCode")}
                        onChange={(e) =>
                          patch({ then: { ...table.then, reasonCode: e.target.value } as RuleThen })
                        }
                      />
                    </span>
                  )}
                </td>
              </tr>
            </tfoot>
          </table>
        </div>
        <p className="text-[11px] text-muted-foreground">{t("cross.hint")}</p>
      </CardContent>
    </Card>
  )
}

/** One fact picker per dimension column, plus the button that adds another. */
function HeaderPickers({
  axis,
  headers,
  facts,
  readOnly,
  onPick,
  onAdd,
  onRemove,
}: {
  axis: "row" | "col"
  headers: CrossHeader[]
  facts: FactRecord[]
  readOnly: boolean
  onPick: (index: number, fact: string) => void
  onAdd: () => void
  onRemove: (index: number) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="mt-0.5 flex flex-wrap items-center gap-1">
      {headers.map((h, i) => (
        <span key={`${h.fact}-${i}`} className="flex items-center gap-0.5">
          <select
            className="max-w-[150px] rounded-sm border border-input bg-transparent px-1 py-0.5 text-[11px]"
            value={h.fact}
            disabled={readOnly}
            onChange={(e) => onPick(i, e.target.value)}
          >
            {facts.map((f) => (
              <option key={f.fact} value={f.fact}>
                {f.displayName ? `${f.displayName} (${f.fact})` : f.fact}
              </option>
            ))}
            {!facts.some((f) => f.fact === h.fact) && <option value={h.fact}>{h.fact}</option>}
          </select>
          {!readOnly && headers.length > 1 && (
            <button
              type="button"
              className="rounded p-0.5 text-muted-foreground hover:text-red-600"
              onClick={() => onRemove(i)}
              aria-label={t(axis === "row" ? "cross.removeRowHeader" : "cross.removeColHeader")}
            >
              <Trash2 className="size-2.5" />
            </button>
          )}
        </span>
      ))}
      {!readOnly && (
        <button
          type="button"
          className="rounded p-0.5 text-muted-foreground hover:text-foreground"
          onClick={onAdd}
          aria-label={t(axis === "row" ? "cross.addRowHeader" : "cross.addColHeader")}
        >
          <Plus className="size-3" />
        </button>
      )}
    </div>
  )
}

/** A dimension condition. No wildcard: see the note at the top of this file. */
function AxisCell({
  fact,
  raw,
  readOnly,
  onChange,
}: {
  fact: FactRecord | undefined
  raw: unknown
  readOnly: boolean
  onChange: (raw: unknown) => void
}) {
  const { t } = useTranslation()
  const draft = cellToDraft(raw)
  const mode: CellMode = draft.mode === "*" ? "eq" : draft.mode
  const committed = draft.mode === "*" ? defaultText(fact) : draft.value
  const [text, setText] = useState(committed)
  const focused = useRef(false)

  useEffect(() => {
    if (!focused.current) setText(committed)
  }, [committed])

  if (readOnly) {
    return (
      <div className="font-mono text-[11px]" dir="ltr">
        {mode === "eq" ? "" : `${mode} `}
        {committed || "—"}
      </div>
    )
  }

  const hint = fact ? `${fact.displayName ?? fact.fact} ${mode} ${committed || "—"}` : undefined

  return (
    <div className="flex items-center gap-0.5" dir="ltr" title={hint}>
      <select
        className="shrink-0 cursor-pointer appearance-none border-0 bg-transparent px-0 text-[10px] text-muted-foreground outline-none focus:ring-1 focus:ring-inset focus:ring-ring"
        value={mode}
        aria-label={t("table.cellMode")}
        onChange={(e) =>
          onChange(draftToCell({ mode: e.target.value as CellMode, value: text || defaultText(fact) }, fact))
        }
      >
        {AXIS_MODES.map((m) => (
          <option key={m} value={m}>
            {m === "eq" ? "=" : m}
          </option>
        ))}
      </select>
      {fact?.type === "boolean" ? (
        <select
          className={cn(BARE, "h-6 w-auto max-w-[150px] cursor-pointer")}
          value={text || "true"}
          onChange={(e) => {
            setText(e.target.value)
            onChange(draftToCell({ mode, value: e.target.value }, fact))
          }}
        >
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      ) : fact?.type === "enum" && fact.enum && fact.enum.length > 0 && mode === "eq" ? (
        <select
          className={cn(BARE, "h-6 w-auto max-w-[150px] cursor-pointer")}
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            onChange(draftToCell({ mode, value: e.target.value }, fact))
          }}
        >
          {fact.enum.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
      ) : (
        <input
          className={cn(BARE, "h-6 min-w-0 max-w-[150px]")}
          value={text}
          onFocus={() => (focused.current = true)}
          onChange={(e) => setText(e.target.value)}
          onBlur={(e) => {
            focused.current = false
            onChange(draftToCell({ mode, value: e.target.value || defaultText(fact) }, fact))
          }}
        />
      )}
    </div>
  )
}

/** One square of the matrix: a literal for `target.var`, or "no rule here". */
function MatrixCell({
  value,
  targetType,
  readOnly,
  onChange,
}: {
  value: TypedLiteral | null
  targetType: string
  readOnly: boolean
  onChange: (v: TypedLiteral | null) => void
}) {
  const { t } = useTranslation()
  const committed = value ? stringify(value.value) : ""
  const [text, setText] = useState(committed)
  const focused = useRef(false)

  useEffect(() => {
    if (!focused.current) setText(committed)
  }, [committed])

  if (readOnly) {
    return (
      <div className="px-1 py-1.5 text-center font-mono text-[11px]" dir="ltr">
        {committed === "" ? "—" : committed}
      </div>
    )
  }

  const commit = (raw: string) => {
    if (raw.trim() === "") return onChange(null)
    onChange({ type: targetType, value: targetType === "integer" ? Number(raw) : raw })
  }

  const clear = (
    <button
      type="button"
      className="absolute end-0.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-muted-foreground opacity-0 hover:text-red-600 group-hover/cell:opacity-100 focus-visible:opacity-100"
      onClick={() => {
        setText("")
        onChange(null)
      }}
      aria-label={t("cross.clear")}
    >
      <Trash2 className="size-3" />
    </button>
  )

  if (targetType === "boolean") {
    return (
      <div className="relative" dir="ltr">
        <select
          className={cn(BARE, "h-8 cursor-pointer text-center")}
          value={committed}
          aria-label={t("cross.cellValue")}
          onChange={(e) =>
            e.target.value === ""
              ? onChange(null)
              : onChange({ type: "boolean", value: e.target.value === "true" })
          }
        >
          <option value="">{t("cross.empty")}</option>
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
        {clear}
      </div>
    )
  }

  return (
    <div className="relative" dir="ltr">
      <input
        className={cn(BARE, "h-8 text-center placeholder:text-center")}
        value={text}
        placeholder={t("cross.empty")}
        aria-label={t("cross.cellValue")}
        onFocus={() => (focused.current = true)}
        onChange={(e) => setText(e.target.value)}
        onBlur={(e) => {
          focused.current = false
          commit(e.target.value)
        }}
      />
      {clear}
    </div>
  )
}

/** Whether any header fact on an axis still has a value not already taken by the existing lines. */
function hasFreeLine(
  headers: CrossHeader[],
  lines: Record<string, unknown>[],
  factIndex: Map<string, FactRecord>
): boolean {
  if (headers.length === 0) return true
  return headers.some((h) => {
    const used = lines.map((l) => l[h.fact])
    const fact = factIndex.get(h.fact)
    if (!fact) return true
    if (fact.type === "enum" && (fact.enum?.length ?? 0) > 0) return fact.enum!.some((v) => !used.includes(v))
    if (fact.type === "boolean") return !(used.includes(true) && used.includes(false))
    return true // integer / string / decimal can always produce one more distinct value
  })
}

/** First value of a fact's own domain, so a renamed dimension never ends up without a condition. */
function sampleFor(fact: FactRecord | undefined): unknown {
  return nextSample(fact, [])
}

/**
 * Whether an existing condition value still means the same thing for the fact it is being moved onto. A value
 * that fails this is not merely odd-looking, it is a cell the compiler will refuse - so it gets replaced.
 */
function fitsValue(prevFact: FactRecord | undefined, nextFact: FactRecord | undefined, value: unknown): boolean {
  if (value === undefined || value === null || value === "") return false
  if (!prevFact || !nextFact || prevFact.type !== nextFact.type) return false
  switch (nextFact.type) {
    case "enum":
      return (nextFact.enum ?? []).includes(String(value))
    case "boolean":
      return typeof value === "boolean"
    case "integer":
      return typeof value === "number" && Number.isInteger(value)
    case "decimal":
      return typeof value === "number" || (typeof value === "string" && value.trim() !== "")
    default:
      return typeof value === "string"
  }
}

/**
 * A concrete condition not yet used on this axis.
 *
 * `used` is the values already taken by the other lines for the same fact. When the domain is exhausted the
 * first value is returned anyway - the author then gets a precise "two lines are identical" diagnostic instead
 * of an empty cell, which would be its own error.
 */
function nextSample(fact: FactRecord | undefined, used: unknown[]): unknown {
  if (!fact) return ""
  if (fact.type === "enum" && (fact.enum?.length ?? 0) > 0) {
    return fact.enum!.find((v) => !used.includes(v)) ?? fact.enum![0]
  }
  if (fact.type === "boolean") {
    if (!used.includes(true)) return true
    if (!used.includes(false)) return false
    return true
  }
  if (fact.type === "integer") return used.length + 1
  return ""
}
