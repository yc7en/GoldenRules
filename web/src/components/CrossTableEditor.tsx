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
 */

const DECISIONS = ["APPROVE", "REJECT", "REVIEW", "ROUTE"]
const TARGET_TYPES = ["string", "enum", "integer", "decimal", "boolean"] as const
const AXIS_MODES = CELL_MODES.filter((m) => m !== "*") as [CellMode, ...CellMode[]]

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
    const nextFact = factIndex.get(fact)
    const nextHeaders = headers.map((h, i) => (i === index ? { fact, type: nextFact?.type } : h))
    // Renaming a dimension moves its condition values instead of leaving orphan keys behind: an undeclared
    // key in a header cell is a hard validation error, so a stale one would make the whole table invalid.
    const nextCells = table[cellKey].map((line) => {
      if (!prev || prev === fact) return line
      const copy = { ...line }
      copy[fact] = copy[prev] ?? sampleFor(nextFact)
      delete copy[prev]
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
            {t("cross.title", { id: table.crossTableId })}
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

        {/* The table-level half of `then`: one conclusion for the whole matrix, plus the assignment target. */}
        <div className="flex flex-wrap items-end gap-3 rounded-md border border-dashed p-2">
          <label className="text-[11px] text-muted-foreground">
            {t("cross.target")}
            <span className="mt-0.5 flex items-center gap-1">
              <input
                className="w-40 rounded-md border border-input bg-transparent px-1 py-1 font-mono text-[11px]"
                dir="ltr"
                value={table.target.var}
                disabled={readOnly}
                aria-label={t("cross.targetVar")}
                onChange={(e) => patch({ target: { ...table.target, var: e.target.value } })}
              />
              <select
                className="rounded-md border border-input bg-transparent px-1 py-1 text-[11px]"
                value={table.target.type}
                disabled={readOnly}
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
          </label>

          <label className="text-[11px] text-muted-foreground">
            {t("cross.then")}
            <span className="mt-0.5 flex items-center gap-1">
              <select
                className="rounded-md border border-input bg-transparent px-1 py-1 text-[11px]"
                value={table.then.decision}
                disabled={readOnly}
                onChange={(e) => patch({ then: { ...table.then, decision: e.target.value } })}
              >
                {DECISIONS.map((d) => (
                  <option key={d} value={d}>
                    {d}
                  </option>
                ))}
              </select>
              <input
                className="w-44 rounded-md border border-input bg-transparent px-1 py-1 font-mono text-[11px]"
                dir="ltr"
                value={table.then.reasonCode}
                disabled={readOnly}
                aria-label={t("table.reasonCode")}
                onChange={(e) =>
                  patch({ then: { ...table.then, reasonCode: e.target.value } as RuleThen })
                }
              />
            </span>
          </label>
        </div>

        <div className="overflow-x-auto">
          <table className="w-full min-w-[560px] border-collapse text-xs">
            <thead>
              <tr>
                <th rowSpan={2} className="min-w-[180px] border-b p-1 text-start align-bottom">
                  <div className="text-[11px] font-medium text-muted-foreground">
                    {t("cross.rowHeaders")}
                  </div>
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
                <th colSpan={cols} className="border-b p-1 text-start">
                  <div className="text-[11px] font-medium text-muted-foreground">
                    {t("cross.colHeaders")}
                  </div>
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
                <th rowSpan={2} className="w-8 border-b p-1" />
              </tr>
              <tr>
                {table.colCells.map((line, j) => (
                  <th key={j} className="min-w-[120px] border-b p-1 align-bottom">
                    {table.colHeaders.map((h) => (
                      <AxisCell
                        key={h.fact}
                        fact={factIndex.get(h.fact)}
                        raw={line[h.fact]}
                        readOnly={readOnly}
                        onChange={(raw) => setAxisCell("col", j, h.fact, raw)}
                      />
                    ))}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {table.rowCells.map((line, i) => (
                <tr key={i} className="border-b align-top">
                  <th className="p-1 text-start">
                    {table.rowHeaders.map((h) => (
                      <AxisCell
                        key={h.fact}
                        fact={factIndex.get(h.fact)}
                        raw={line[h.fact]}
                        readOnly={readOnly}
                        onChange={(raw) => setAxisCell("row", i, h.fact, raw)}
                      />
                    ))}
                  </th>
                  {table.colCells.map((_, j) => (
                    <td key={j} className="p-1">
                      <MatrixCell
                        value={table.cells[i]?.[j] ?? null}
                        targetType={table.target.type}
                        readOnly={readOnly}
                        onChange={(v) => setCell(i, j, v)}
                      />
                    </td>
                  ))}
                  <td className="p-1">
                    {!readOnly && rows > 1 && (
                      <button
                        type="button"
                        className="rounded p-0.5 text-muted-foreground hover:text-red-600"
                        onClick={() => removeRow(i)}
                        aria-label={t("cross.removeRow")}
                      >
                        <Trash2 className="size-3" />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
              {!readOnly && cols > 1 && (
                <tr>
                  <td />
                  {table.colCells.map((_, j) => (
                    <td key={j} className="p-1 text-center">
                      <button
                        type="button"
                        className="rounded p-0.5 text-muted-foreground hover:text-red-600"
                        onClick={() => removeCol(j)}
                        aria-label={t("cross.removeCol")}
                      >
                        <Trash2 className="size-3" />
                      </button>
                    </td>
                  ))}
                  <td />
                </tr>
              )}
            </tbody>
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
            className="max-w-[150px] rounded-md border border-input bg-transparent px-1 py-0.5 text-[11px]"
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

  return (
    <div className="flex items-center gap-0.5" dir="ltr">
      <select
        className="rounded-md border border-input bg-transparent px-0.5 py-0.5 text-[10px]"
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
          className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
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
          className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
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
          className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
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
      <div className="font-mono text-[11px]" dir="ltr">
        {committed === "" ? "—" : committed}
      </div>
    )
  }

  const commit = (raw: string) => {
    if (raw.trim() === "") return onChange(null)
    onChange({ type: targetType, value: targetType === "integer" ? Number(raw) : raw })
  }

  if (targetType === "boolean") {
    return (
      <div className="flex items-center gap-0.5" dir="ltr">
        <select
          className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
          value={committed}
          aria-label={t("cross.cellValue")}
          onChange={(e) => (e.target.value === "" ? onChange(null) : onChange({ type: "boolean", value: e.target.value === "true" }))}
        >
          <option value="">{t("cross.empty")}</option>
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      </div>
    )
  }

  return (
    <div className="flex items-center gap-0.5" dir="ltr">
      <input
        className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
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
      <button
        type="button"
        className="rounded p-0.5 text-muted-foreground hover:text-red-600"
        onClick={() => {
          setText("")
          onChange(null)
        }}
        aria-label={t("cross.clear")}
      >
        <Trash2 className="size-3" />
      </button>
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
