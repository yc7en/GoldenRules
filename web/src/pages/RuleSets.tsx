import { useTranslation } from "react-i18next"
import { Link } from "react-router-dom"
import { Plus } from "lucide-react"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Async } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api } from "@/lib/api"

export default function RuleSets() {
  const { t } = useTranslation()
  const state = useAsync(() => api.rulesets.list(), [])

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold">{t("rulesets.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("rulesets.subtitle")}</p>
        </div>
        <Link to="/rulesets/new">
          <Button size="sm" className="shrink-0">
            <Plus className="size-3.5" />
            {t("rulesets.new")}
          </Button>
        </Link>
      </div>

      <Async
        state={state}
        loadingLabel={t("rulesets.loading")}
        empty={(d) => (d.items ?? []).length === 0}
      >
        {(data) => (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("rulesets.col.id")}</TableHead>
                <TableHead>{t("rulesets.col.scene")}</TableHead>
                <TableHead>{t("rulesets.col.version")}</TableHead>
                <TableHead>{t("rulesets.col.hitStrategy")}</TableHead>
                <TableHead>{t("rulesets.col.default")}</TableHead>
                <TableHead>{t("rulesets.col.ruleCount")}</TableHead>
                <TableHead>{t("rulesets.col.tests")}</TableHead>
                <TableHead>{t("rulesets.col.updatedAt")}</TableHead>
                <TableHead className="text-end">{t("common.actions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.items.map((rs) => {
                const tests = rs.tests ?? []
                const approved = tests.filter((t) => t.status === "approved").length
                return (
                  <TableRow key={rs.rulesetId}>
                    <TableCell className="font-medium">{rs.rulesetId}</TableCell>
                    <TableCell>
                      <Badge variant="secondary">{rs.scene}</Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{rs.version}</TableCell>
                    <TableCell>{rs.hitStrategy}</TableCell>
                    <TableCell className="text-xs">{rs.defaultDecision}</TableCell>
                    {/* `ruleCount` comes from the server: a decision table / cross table stores an empty
                        `rules` list and would otherwise always read 0 here. */}
                    <TableCell>{rs.ruleCount ?? rs.rules?.length ?? 0}</TableCell>
                    <TableCell>
                      <span
                        className={
                          tests.length > 0 && approved === 0
                            ? "text-amber-700"
                            : ""
                        }
                        title={
                          tests.length > 0 && approved === 0
                            ? t("rulesets.unapprovedTitle")
                            : undefined
                        }
                      >
                        {approved} / {tests.length}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {/* Timestamp plus actor: both are ASCII, so the pair is pinned to LTR -
                          inside an RTL paragraph the two numeric runs would swap order. */}
                      <span dir="ltr">
                        {rs.updatedAt
                          ? `${formatTime(rs.updatedAt)} · ${rs.updatedBy ?? "?"}`
                          : "—"}
                      </span>
                    </TableCell>
                    <TableCell className="text-end">
                      <Link to={`/rulesets/${encodeURIComponent(rs.rulesetId)}`}>
                        <Button variant="outline" size="sm">
                          {t("common.edit")}
                        </Button>
                      </Link>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Async>
    </div>
  )
}

/**
 * Timestamps stay `YYYY-MM-DD HH:mm` in every locale on purpose: they are compared as strings
 * across logs and screenshots, so a localized format would make two views of the same event
 * impossible to line up.
 */
function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(
    d.getHours()
  )}:${pad(d.getMinutes())}`
}
