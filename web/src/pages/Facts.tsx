import { useState } from "react"
import { useTranslation } from "react-i18next"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Async } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api } from "@/lib/api"
import type { FactRecord } from "@/lib/api-types"

/**
 * Contract enum values (`PII`, `DEGRADE`, ...) are never translated - they appear in logs,
 * exports and API responses. Only the display label next to them changes with the locale.
 */
const SENSITIVITY_KEYS = {
  PUBLIC: "facts.sensitivity.PUBLIC",
  INTERNAL: "facts.sensitivity.INTERNAL",
  PII: "facts.sensitivity.PII",
  SECRET: "facts.sensitivity.SECRET",
} as const

const ON_MISSING_KEYS = {
  ERROR: "facts.onMissing.ERROR",
  DEFAULT: "facts.onMissing.DEFAULT",
  NULL: "facts.onMissing.NULL",
  DEGRADE: "facts.onMissing.DEGRADE",
} as const

export default function Facts() {
  const { t } = useTranslation()
  const state = useAsync(() => api.facts.list(), [])
  const [q, setQ] = useState("")

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold">{t("facts.title")}</h1>
          <p className="text-sm text-muted-foreground">
            {t("facts.subtitle")}
            {state.data && ` · ${t("facts.catalogVersion", { version: state.data.catalogVersion })}`}
            {state.data && ` · ${t("facts.fieldCount", { count: state.data.items.length })}`}
          </p>
        </div>
        <input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder={t("facts.searchPlaceholder")}
          className="w-56 shrink-0 rounded-md border border-input bg-transparent px-2 py-1.5 text-sm"
        />
      </div>

      <div className="rounded-md border border-blue-300 bg-blue-50 px-3 py-2 text-xs text-blue-800">
        {t("facts.notice")}
      </div>

      <Async
        state={state}
        loadingLabel={t("facts.loading")}
        empty={(d) => d.items.length === 0}
      >
        {(data) => {
          const rows = data.items.filter(
            (f) =>
              !q ||
              f.fact.toLowerCase().includes(q.toLowerCase()) ||
              (f.semantics ?? "").includes(q) ||
              (f.displayName ?? "").includes(q)
          )
          if (rows.length === 0) {
            return (
              <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                {t("facts.noMatch", { q })}
              </div>
            )
          }
          return (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("facts.col.fact")}</TableHead>
                  <TableHead>{t("facts.col.displayName")}</TableHead>
                  <TableHead>{t("facts.col.type")}</TableHead>
                  <TableHead>{t("facts.col.required")}</TableHead>
                  <TableHead>{t("facts.col.nullable")}</TableHead>
                  <TableHead>{t("facts.col.onMissing")}</TableHead>
                  <TableHead>{t("facts.col.sensitivity")}</TableHead>
                  <TableHead>{t("facts.col.version")}</TableHead>
                  <TableHead>{t("facts.col.semantics")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((f) => (
                  <FactRow key={f.fact} fact={f} />
                ))}
              </TableBody>
            </Table>
          )
        }}
      </Async>
    </div>
  )
}

function FactRow({ fact: f }: { fact: FactRecord }) {
  const { t } = useTranslation()
  const sensKey =
    SENSITIVITY_KEYS[(f.sensitivity ?? "PUBLIC") as keyof typeof SENSITIVITY_KEYS] ??
    SENSITIVITY_KEYS.PUBLIC
  const missingKey = ON_MISSING_KEYS[(f.onMissing ?? "") as keyof typeof ON_MISSING_KEYS]

  return (
    <TableRow>
      <TableCell className="font-mono text-sm">
        {f.fact}
        {f.deprecated && (
          <Badge className="ms-2 bg-muted text-muted-foreground">
            {t("facts.deprecated")}
          </Badge>
        )}
      </TableCell>
      <TableCell className="text-sm">{f.displayName ?? "—"}</TableCell>
      <TableCell>
        <Badge variant="outline">{f.type}</Badge>
        {f.enum && (
          <span className="ms-1 text-xs text-muted-foreground">
            {f.enum.join("/")}
          </span>
        )}
      </TableCell>
      <TableCell>{f.required ? t("common.yes") : t("common.no")}</TableCell>
      <TableCell>{f.nullable ? t("common.yes") : t("common.no")}</TableCell>
      <TableCell className="text-xs">
        {missingKey ? t(missingKey) : f.onMissing ?? "—"}
        {f.hasDefault && f.default !== undefined && (
          <span className="ms-1 font-mono text-muted-foreground">
            ({JSON.stringify(f.default)})
          </span>
        )}
      </TableCell>
      <TableCell>
        <Badge variant="secondary">{t(sensKey)}</Badge>
      </TableCell>
      <TableCell className="text-xs">{f.version}</TableCell>
      <TableCell className="max-w-[28rem] text-sm text-muted-foreground">
        {f.semantics ?? "—"}
      </TableCell>
    </TableRow>
  )
}
