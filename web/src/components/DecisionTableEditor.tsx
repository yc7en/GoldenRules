import { useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { Plus, Trash2 } from "lucide-react"
import type { DecisionTableDef, FactRecord, RuleThen } from "@/lib/api-types"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { cn } from "@/lib/utils"

/**
 * Decision table editor.
 *
 * A table is one of the two **stored** forms (`contracts/01` §3.2.4): a row is a rule, the conditions
 * across a row are and-ed, and row order is priority order. It therefore writes `table` and nothing
 * else - the expression rules are produced by the backend at compile time, never stored alongside.
 */

/** `*` is a wildcard; the rest become `eq` (bare literal) or `{op, value}` cells. */
const CELL_MODES = ["*", "eq", "ne", "gt", "gte", "lt", "lte"] as const
type CellMode = (typeof CELL_MODES)[number]

const DECISIONS = ["APPROVE", "REJECT", "REVIEW", "ROUTE"]

export interface DecisionTableEditorProps {
  table: DecisionTableDef
  facts: FactRecord[]
  onChange: (table: DecisionTableDef) => void
  readOnlyReason?: string
}

export function DecisionTableEditor({ table, facts, onChange, readOnlyReason }: DecisionTableEditorProps) {
  const { t } = useTranslation()
  const readOnly = !!readOnlyReason
  const factIndex = new Map(facts.map((f) => [f.fact, f]))

  const setColumn = (index: number, fact: string) => {
    const prev = table.columns[index]
    const nextFact = factIndex.get(fact)
    const columns = table.columns.map((c, i) => (i === index ? { fact, type: nextFact?.type } : c))
    // Keep row data consistent with the renamed column instead of leaving stale keys behind: an
    // undeclared key in `conditions` is a hard validation error.
    const rows = table.rows.map((row) => {
      const conditions = { ...row.conditions }
      if (prev && prev.fact !== fact) {
        conditions[fact] = conditions[prev.fact] ?? "*"
        delete conditions[prev.fact]
      }
      return { ...row, conditions }
    })
    onChange({ ...table, columns, rows })
  }

  const addColumn = () => {
    const used = new Set(table.columns.map((c) => c.fact))
    const next = facts.find((f) => !used.has(f.fact))
    if (!next) return
    onChange({
      ...table,
      columns: [...table.columns, { fact: next.fact, type: next.type }],
      rows: table.rows.map((row) => ({
        ...row,
        conditions: { ...row.conditions, [next.fact]: "*" },
      })),
    })
  }

  const removeColumn = (index: number) => {
    const dropped = table.columns[index]
    onChange({
      ...table,
      columns: table.columns.filter((_, i) => i !== index),
      rows: table.rows.map((row) => {
        const conditions = { ...row.conditions }
        if (dropped) delete conditions[dropped.fact]
        return { ...row, conditions }
      }),
    })
  }

  const addRow = () => {
    const conditions: Record<string, unknown> = {}
    for (const col of table.columns) conditions[col.fact] = "*"
    onChange({
      ...table,
      rows: [...table.rows, { conditions, then: { decision: "REVIEW", reasonCode: "DEFAULT_REVIEW" } }],
    })
  }

  const removeRow = (index: number) => {
    onChange({ ...table, rows: table.rows.filter((_, i) => i !== index) })
  }

  const setCell = (rowIndex: number, fact: string, raw: unknown) => {
    onChange({
      ...table,
      rows: table.rows.map((row, i) =>
        i === rowIndex ? { ...row, conditions: { ...row.conditions, [fact]: raw } } : row
      ),
    })
  }

  const setThen = (rowIndex: number, then: RuleThen) => {
    onChange({
      ...table,
      rows: table.rows.map((row, i) => (i === rowIndex ? { ...row, then } : row)),
    })
  }

  return (
    <Card>
      <CardHeader className="py-2">
        <CardTitle className="flex flex-wrap items-center justify-between gap-2 text-sm">
          <span>
            {t("table.title", { tableId: table.tableId })}
            <span className="ms-2 text-[11px] font-normal text-muted-foreground">
              {t("table.shape", { columns: table.columns.length, rows: table.rows.length })}
            </span>
          </span>
          {!readOnly && (
            <span className="flex gap-2">
              <Button variant="outline" size="sm" onClick={addColumn}>
                <Plus className="size-3.5" />
                {t("table.addColumn")}
              </Button>
              <Button variant="outline" size="sm" onClick={addRow}>
                <Plus className="size-3.5" />
                {t("table.addRow")}
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
          <table className="w-full min-w-[640px] border-collapse text-xs">
            <thead>
              <tr className="border-b">
                <th className="w-8 p-1 text-start font-medium text-muted-foreground">#</th>
                {table.columns.map((col, i) => (
                  <th key={`${col.fact}-${i}`} className="min-w-[160px] p-1 text-start">
                    <div className="flex items-center gap-1">
                      <select
                        className="w-full rounded-md border border-input bg-transparent px-1 py-1 text-[11px]"
                        value={col.fact}
                        disabled={readOnly}
                        onChange={(e) => setColumn(i, e.target.value)}
                      >
                        {facts.map((f) => (
                          <option key={f.fact} value={f.fact}>
                            {f.displayName ? `${f.displayName} (${f.fact})` : f.fact}
                          </option>
                        ))}
                        {!factIndex.has(col.fact) && <option value={col.fact}>{col.fact}</option>}
                      </select>
                      {!readOnly && (
                        <button
                          type="button"
                          className="rounded p-0.5 text-muted-foreground hover:text-red-600"
                          onClick={() => removeColumn(i)}
                          aria-label={t("table.removeColumn")}
                        >
                          <Trash2 className="size-3" />
                        </button>
                      )}
                    </div>
                    <div className="mt-0.5 font-mono text-[10px] font-normal text-muted-foreground" dir="ltr">
                      {col.type ?? "?"}
                    </div>
                  </th>
                ))}
                <th className="min-w-[120px] p-1 text-start font-medium text-muted-foreground">
                  {t("table.decision")}
                </th>
                <th className="min-w-[160px] p-1 text-start font-medium text-muted-foreground">
                  {t("table.reasonCode")}
                </th>
                <th className="w-8 p-1" />
              </tr>
            </thead>
            <tbody>
              {table.rows.map((row, rowIndex) => (
                <tr key={rowIndex} className="border-b align-top">
                  <td className="p-1 text-[11px] text-muted-foreground">{rowIndex + 1}</td>
                  {table.columns.map((col, i) => (
                    <td key={`${col.fact}-${i}`} className="p-1">
                      <CellEditor
                        fact={factIndex.get(col.fact)}
                        raw={row.conditions[col.fact]}
                        readOnly={readOnly}
                        onChange={(raw) => setCell(rowIndex, col.fact, raw)}
                      />
                    </td>
                  ))}
                  <td className="p-1">
                    <select
                      className={cn(
                        "w-full rounded-md border border-input bg-transparent px-1 py-1 text-[11px]"
                      )}
                      value={row.then.decision}
                      disabled={readOnly}
                      onChange={(e) => setThen(rowIndex, { ...row.then, decision: e.target.value })}
                    >
                      {DECISIONS.map((d) => (
                        <option key={d} value={d}>
                          {d}
                        </option>
                      ))}
                    </select>
                  </td>
                  <td className="p-1">
                    <ReasonInput
                      value={row.then.reasonCode}
                      readOnly={readOnly}
                      onChange={(reasonCode) => setThen(rowIndex, { ...row.then, reasonCode })}
                    />
                  </td>
                  <td className="p-1">
                    {!readOnly && table.rows.length > 1 && (
                      <button
                        type="button"
                        className="rounded p-0.5 text-muted-foreground hover:text-red-600"
                        onClick={() => removeRow(rowIndex)}
                        aria-label={t("table.removeRow")}
                      >
                        <Trash2 className="size-3" />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="text-[11px] text-muted-foreground">{t("table.hint")}</p>
      </CardContent>
    </Card>
  )
}

function CellEditor({
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
  const mode = draft.mode

  const setMode = (next: CellMode) => {
    if (next === "*") return onChange("*")
    onChange(draftToCell({ mode: next, value: draft.value || defaultText(fact) }, fact))
  }

  return (
    <div className="space-y-0.5">
      <select
        className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 text-[11px]"
        value={mode}
        disabled={readOnly}
        onChange={(e) => setMode(e.target.value as CellMode)}
        aria-label={t("table.cellMode")}
      >
        {CELL_MODES.map((m) => (
          <option key={m} value={m}>
            {m === "*" ? t("table.any") : m}
          </option>
        ))}
      </select>
      {mode !== "*" && (
        <ValueInput
          fact={fact}
          mode={mode}
          value={draft.value}
          readOnly={readOnly}
          onChange={(value) => onChange(draftToCell({ mode, value }, fact))}
        />
      )}
    </div>
  )
}

/** Local draft while focused: committing every keystroke would turn "12" into "1" then "12". */
function ValueInput({
  fact,
  mode,
  value,
  readOnly,
  onChange,
}: {
  fact: FactRecord | undefined
  mode: CellMode
  value: string
  readOnly: boolean
  onChange: (value: string) => void
}) {
  const [text, setText] = useState(value)
  const focused = useRef(false)
  useEffect(() => {
    if (!focused.current) setText(value)
  }, [value])

  if (fact?.type === "enum" && fact.enum && fact.enum.length > 0 && mode === "eq") {
    return (
      <select
        className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
        value={value}
        disabled={readOnly}
        onChange={(e) => onChange(e.target.value)}
        dir="ltr"
      >
        {fact.enum.map((v) => (
          <option key={v} value={v}>
            {v}
          </option>
        ))}
      </select>
    )
  }

  if (fact?.type === "boolean") {
    return (
      <select
        className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
        value={value || "true"}
        disabled={readOnly}
        onChange={(e) => onChange(e.target.value)}
        dir="ltr"
      >
        <option value="true">true</option>
        <option value="false">false</option>
      </select>
    )
  }

  return (
    <input
      className="w-full rounded-md border border-input bg-transparent px-1 py-0.5 font-mono text-[11px]"
      value={text}
      dir="ltr"
      disabled={readOnly}
      onFocus={() => (focused.current = true)}
      onChange={(e) => setText(e.target.value)}
      onBlur={() => {
        focused.current = false
        onChange(text)
      }}
    />
  )
}

function ReasonInput({
  value,
  readOnly,
  onChange,
}: {
  value: string
  readOnly: boolean
  onChange: (v: string) => void
}) {
  const [text, setText] = useState(value)
  const focused = useRef(false)
  useEffect(() => {
    if (!focused.current) setText(value)
  }, [value])
  return (
    <input
      className="w-full rounded-md border border-input bg-transparent px-1 py-1 font-mono text-[11px]"
      value={text}
      dir="ltr"
      disabled={readOnly}
      onFocus={() => (focused.current = true)}
      onChange={(e) => setText(e.target.value)}
      onBlur={() => {
        focused.current = false
        onChange(text)
      }}
    />
  )
}

// ---------------------------------------------------------------- cell <-> draft

interface CellDraft {
  mode: CellMode
  value: string
}

function cellToDraft(raw: unknown): CellDraft {
  if (raw === undefined) return { mode: "*", value: "" }
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

function draftToCell(draft: CellDraft, fact: FactRecord | undefined): unknown {
  if (draft.mode === "*") return "*"
  const value = literalValue(draft.value, fact)
  if (draft.mode === "eq") return value
  return { op: draft.mode, value }
}

function literalValue(raw: string, fact: FactRecord | undefined): unknown {
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

function stringify(v: unknown): string {
  if (v === null || v === undefined) return ""
  if (typeof v === "object") return JSON.stringify(v)
  return String(v)
}

function defaultText(fact: FactRecord | undefined): string {
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
