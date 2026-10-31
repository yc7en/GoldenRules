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
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Async, ErrorBox } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api, ApiError } from "@/lib/api"
import type { RulesetDefinition, TestCaseRecord } from "@/lib/api-types"

const AUTHOR_KEYS = {
  agent: "tests.author.agent",
  engineer: "tests.author.engineer",
  business: "tests.author.business",
} as const

const statusConfig: Record<
  TestCaseRecord["status"],
  { labelKey: "tests.status.pending_review" | "tests.status.approved" | "tests.status.rejected"; cls: string }
> = {
  pending_review: {
    labelKey: "tests.status.pending_review",
    cls: "bg-amber-100 text-amber-700",
  },
  approved: { labelKey: "tests.status.approved", cls: "bg-green-100 text-green-700" },
  rejected: { labelKey: "tests.status.rejected", cls: "bg-red-100 text-red-700" },
}

interface Row extends TestCaseRecord {
  rulesetId: string
  rulesetVersion: string
}

export default function TestCases() {
  const { t } = useTranslation()
  const state = useAsync(() => api.rulesets.list(), [])
  const [filter, setFilter] = useState<"pending_review" | "all">("pending_review")
  const [busy, setBusy] = useState<string | null>(null)
  const [actionError, setActionError] = useState<ApiError | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const apply = async (
    rs: RulesetDefinition,
    testId: string,
    status: TestCaseRecord["status"]
  ) => {
    const key = `${rs.rulesetId}/${testId}`
    setBusy(key)
    setActionError(null)
    setNotice(null)
    try {
      // Test case status lives inside the ruleset, so the whole ruleset has to be PUT back - the backend
      // also runs a dry-run on the way, which means this step doubles as proof that "this ruleset still compiles".
      const fresh = await api.rulesets.get(rs.rulesetId)
      const tests = (fresh.tests ?? []).map((tc) =>
        tc.testId === testId ? { ...tc, status } : tc
      )
      const report = await api.rulesets.save(rs.rulesetId, { ...fresh, tests })
      if (!report.valid) {
        const first = report.errors[0]
        setActionError(
          new ApiError(
            422,
            first?.code ?? "VALIDATION_FAILED",
            t("tests.saveRejected", {
              message: first?.message ?? "—",
              path: first?.path ?? "",
            }),
            report
          )
        )
        return
      }
      setNotice(
        t("tests.saved", {
          testId,
          status: t(statusConfig[status].labelKey),
          rulesetId: rs.rulesetId,
        })
      )
      state.reload()
    } catch (e) {
      setActionError(
        e instanceof ApiError ? e : new ApiError(0, "UNKNOWN", String(e))
      )
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold">{t("tests.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("tests.subtitle")}</p>
        </div>
        <div className="flex shrink-0 gap-2">
          <Button
            variant={filter === "pending_review" ? "default" : "outline"}
            size="sm"
            onClick={() => setFilter("pending_review")}
          >
            {t("tests.pending")}
          </Button>
          <Button
            variant={filter === "all" ? "default" : "outline"}
            size="sm"
            onClick={() => setFilter("all")}
          >
            {t("common.all")}
          </Button>
        </div>
      </div>

      <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
        {t("tests.notice")}
      </div>

      {actionError && <ErrorBox error={actionError} />}
      {notice && (
        <div className="rounded-md border border-green-300 bg-green-50 px-3 py-2 text-xs text-green-800">
          {notice}
        </div>
      )}

      <Async state={state} loadingLabel={t("tests.loading")}>
        {(data) => {
          const rows = flatten(data.items)
          const filtered =
            filter === "all" ? rows : rows.filter((r) => r.status === filter)
          if (filtered.length === 0) {
            return (
              <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                {filter === "pending_review"
                  ? t("tests.emptyPending")
                  : t("tests.emptyAll")}
              </div>
            )
          }
          return (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("tests.col.testId")}</TableHead>
                  <TableHead>{t("tests.col.ruleset")}</TableHead>
                  <TableHead>{t("tests.col.scene")}</TableHead>
                  <TableHead>{t("tests.col.input")}</TableHead>
                  <TableHead>{t("tests.col.expect")}</TableHead>
                  <TableHead>{t("tests.col.author")}</TableHead>
                  <TableHead>{t("tests.col.status")}</TableHead>
                  <TableHead className="text-end">{t("common.actions")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((c) => {
                  const rs = data.items.find((r) => r.rulesetId === c.rulesetId)!
                  const key = `${c.rulesetId}/${c.testId}`
                  const authorKey =
                    AUTHOR_KEYS[c.author as keyof typeof AUTHOR_KEYS]
                  return (
                    <TableRow key={key}>
                      <TableCell className="font-medium">
                        {c.testId}
                        {c.rulesetId && (
                          <div className="text-[10px] font-normal text-muted-foreground">
                            {c.rulesetId}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        {c.rulesetId}
                        <div className="text-[10px] text-muted-foreground">
                          {c.rulesetVersion}
                        </div>
                      </TableCell>
                      <TableCell>
                        <Badge variant="secondary">{c.scene}</Badge>
                      </TableCell>
                      <TableCell className="max-w-[16rem] truncate font-mono text-xs">
                        {JSON.stringify(c.input)}
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline">{c.expect.decision}</Badge>
                        {c.expect.reasonCode && (
                          <code className="ms-1 text-xs">{c.expect.reasonCode}</code>
                        )}
                      </TableCell>
                      <TableCell>
                        {authorKey ? t(authorKey) : c.author}
                      </TableCell>
                      <TableCell>
                        <Badge className={statusConfig[c.status].cls}>
                          {t(statusConfig[c.status].labelKey)}
                        </Badge>
                      </TableCell>
                      <TableCell className="text-end">
                        <div className="flex justify-end gap-1">
                          {c.status !== "approved" && (
                            <Button
                              size="sm"
                              disabled={busy === key}
                              onClick={() => apply(rs, c.testId, "approved")}
                            >
                              {busy === key ? t("tests.saving") : t("tests.confirm")}
                            </Button>
                          )}
                          {c.status !== "rejected" && (
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={busy === key}
                              onClick={() => apply(rs, c.testId, "rejected")}
                            >
                              {t("tests.reject")}
                            </Button>
                          )}
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )
        }}
      </Async>
    </div>
  )
}

function flatten(items: RulesetDefinition[]): Row[] {
  const out: Row[] = []
  for (const rs of items) {
    for (const t of rs.tests ?? []) {
      out.push({
        ...t,
        // When the test case does not carry its own rulesetId, fill it in from the host ruleset - the list must make clear which ruleset it belongs to
        rulesetId: t.rulesetId ?? rs.rulesetId,
        rulesetVersion: rs.version,
      })
    }
  }
  return out
}
