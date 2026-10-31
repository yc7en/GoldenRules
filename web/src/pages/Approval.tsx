import { useState } from "react"
import { useTranslation } from "react-i18next"
import type { TFunction } from "i18next"
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
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Async, ErrorBox, Loading } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api, ApiError, getActor } from "@/lib/api"
import type {
  ChangeDetail,
  ChangeSummary,
  RiskLevel,
} from "@/lib/api-types"

const STATUS_KEYS = {
  draft: "approval.status.draft",
  submitted: "approval.status.submitted",
  approval: "approval.status.approval",
  approved: "approval.status.approved",
  rejected: "approval.status.rejected",
  published: "approval.status.published",
} as const

const STATUS_CLS: Record<string, string> = {
  draft: "bg-muted text-muted-foreground",
  submitted: "bg-blue-100 text-blue-700",
  approval: "bg-amber-100 text-amber-700",
  approved: "bg-blue-100 text-blue-700",
  rejected: "bg-red-100 text-red-700",
  published: "bg-green-100 text-green-700",
}

const SUBMITTER_KEYS = {
  engineer: "approval.submitter.engineer",
  business: "approval.submitter.business",
  agent: "approval.submitter.agent",
  risk: "approval.submitter.risk",
  compliance: "approval.submitter.compliance",
  owner: "approval.submitter.owner",
} as const

/** Role names are an internal vocabulary, so they get a label; the value itself is never touched. */
function submitterLabel(t: TFunction, s: string): string {
  const key = SUBMITTER_KEYS[s as keyof typeof SUBMITTER_KEYS]
  return key ? t(key) : s
}

function statusLabel(t: TFunction, status: string): string {
  const key = STATUS_KEYS[status as keyof typeof STATUS_KEYS]
  return key ? t(key) : status
}

export default function Approval() {
  const { t } = useTranslation()
  const changes = useAsync(() => api.changes.list(), [])
  const matrix = useAsync(() => api.changes.matrix(), [])

  return (
    <div className="p-6 space-y-4">
      <div>
        <h1 className="text-lg font-semibold">{t("approval.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("approval.subtitle")}</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">{t("approval.matrixTitle")}</CardTitle>
        </CardHeader>
        <CardContent>
          <Async state={matrix} loadingLabel={t("approval.loadingMatrix")}>
            {(m) => <MatrixTable cells={m.cells} />}
          </Async>
          <p className="mt-2 text-xs text-red-600">{t("approval.hardRule")}</p>
        </CardContent>
      </Card>

      <Async state={changes} loadingLabel={t("approval.loadingChanges")}>
        {(data) =>
          data.items.length === 0 ? (
            <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
              {t("approval.empty")}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("approval.col.change")}</TableHead>
                  <TableHead>{t("approval.col.package")}</TableHead>
                  <TableHead>{t("approval.col.risk")}</TableHead>
                  <TableHead>{t("approval.col.submitter")}</TableHead>
                  <TableHead>{t("approval.col.progress")}</TableHead>
                  <TableHead>{t("approval.col.sla")}</TableHead>
                  <TableHead>{t("approval.col.status")}</TableHead>
                  <TableHead className="text-end">{t("common.actions")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.items.map((c) => (
                  <ChangeRow
                    key={c.changeId}
                    change={c}
                    onChanged={() => {
                      changes.reload()
                      matrix.reload()
                    }}
                  />
                ))}
              </TableBody>
            </Table>
          )
        }
      </Async>
    </div>
  )
}

function MatrixTable({ cells }: { cells: { riskLevel: RiskLevel; submitterType: string; requiredCount: number; slotLabels: string[]; forbidden: boolean; forbiddenCause?: string }[] }) {
  const { t } = useTranslation()
  const levels: RiskLevel[] = ["L1", "L2", "L3", "L4"]
  const submitters = [...new Set(cells.map((c) => c.submitterType))]
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("approval.col.risk")}</TableHead>
          {submitters.map((s) => (
            <TableHead key={s}>{submitterLabel(t, s)}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {levels.map((lvl) => (
          <TableRow key={lvl}>
            <TableCell>
              <Badge className={`risk-badge-${lvl}`}>{lvl}</Badge>
            </TableCell>
            {submitters.map((s) => {
              const cell = cells.find(
                (c) => c.riskLevel === lvl && c.submitterType === s
              )
              if (!cell) return <TableCell key={s}>—</TableCell>
              return (
                <TableCell key={s} className="text-xs">
                  {cell.forbidden ? (
                    <span className="font-medium text-red-600">
                      {t("approval.forbidden")}
                      {cell.forbiddenCause && (
                        <span className="ms-1 font-normal text-red-500">
                          ({cell.forbiddenCause})
                        </span>
                      )}
                    </span>
                  ) : (
                    <span>
                      {t("approval.people", { count: cell.requiredCount })} ·{" "}
                      {cell.slotLabels.join(" / ")}
                    </span>
                  )}
                </TableCell>
              )
            })}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

function ChangeRow({
  change,
  onChanged,
}: {
  change: ChangeSummary
  onChanged: () => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [detail, setDetail] = useState<ChangeDetail | null>(null)
  const [detailError, setDetailError] = useState<ApiError | null>(null)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState<ApiError | null>(null)
  const [role, setRole] = useState("")
  const [comment, setComment] = useState("")
  const [flash, setFlash] = useState<string | null>(null)

  const loadDetail = async () => {
    setDetailError(null)
    try {
      const d = await api.changes.get(change.changeId)
      setDetail(d)
      const unfilled = unfilledRoles(d)
      setRole(unfilled[0]?.[1] ?? "")
    } catch (e) {
      setDetailError(e instanceof ApiError ? e : new ApiError(0, "UNKNOWN", String(e)))
    }
  }

  const toggle = async () => {
    const next = !open
    setOpen(next)
    if (next && !detail) await loadDetail()
  }

  const act = async (kind: "approve" | "reject" | "publish") => {
    setBusy(true)
    setActionError(null)
    setFlash(null)
    try {
      if (kind === "approve") {
        const r = await api.changes.approve(change.changeId, role, comment)
        setFlash(t("approval.flashApproved", { role, remaining: r.remaining }))
      } else if (kind === "reject") {
        await api.changes.reject(change.changeId, role, comment)
        setFlash(t("approval.flashRejected"))
      } else {
        const r = await api.packages.publish(
          change.packageId,
          change.packageVersion,
          change.changeId
        )
        setFlash(
          t("approval.flashPublished", {
            packageId: r.version.packageId,
            packageVersion: r.version.packageVersion,
            status: r.version.status,
          })
        )
      }
      setComment("")
      await loadDetail()
      onChanged()
    } catch (e) {
      setActionError(e instanceof ApiError ? e : new ApiError(0, "UNKNOWN", String(e)))
    } finally {
      setBusy(false)
    }
  }

  const actor = getActor()
  const pending = change.status === "submitted" || change.status === "approval"
  const canPublish = change.status === "approved"

  return (
    <>
      <TableRow>
        <TableCell className="font-mono text-xs">
          {change.changeId}
          <div className="text-[10px] font-normal text-muted-foreground" dir="ltr">
            {formatTime(change.createdAt)}
          </div>
        </TableCell>
        <TableCell className="text-xs">
          {change.packageId}
          <span className="ms-1 text-muted-foreground">{change.packageVersion}</span>
          <div className="text-[10px] text-muted-foreground">{change.changeType}</div>
        </TableCell>
        <TableCell>
          <Badge className={`risk-badge-${change.riskLevel}`}>{change.riskLevel}</Badge>
        </TableCell>
        <TableCell className="text-xs">
          {change.submitter}
          <div className="text-[10px] text-muted-foreground">
            {submitterLabel(t, change.submitterType)}
          </div>
        </TableCell>
        <TableCell className="text-xs">
          {change.approved} / {change.required}
          {change.remaining > 0 && (
            <span className="ms-1 text-amber-700">
              {t("approval.remaining", { count: change.remaining })}
            </span>
          )}
        </TableCell>
        <TableCell className="text-xs">
          <SlaCell sla={change.sla} />
        </TableCell>
        <TableCell>
          <Badge className={STATUS_CLS[change.status] ?? ""}>
            {statusLabel(t, change.status)}
          </Badge>
        </TableCell>
        <TableCell className="text-end">
          <div className="flex justify-end gap-1">
            <Button size="sm" variant="outline" onClick={toggle}>
              {open ? t("common.collapse") : t("common.detail")}
            </Button>
            {canPublish && (
              <Button size="sm" disabled={busy} onClick={() => act("publish")}>
                {t("approval.publish")}
              </Button>
            )}
          </div>
        </TableCell>
      </TableRow>

      {open && (
        <TableRow>
          <TableCell colSpan={8} className="bg-muted/40">
            {detailError && <ErrorBox error={detailError} onRetry={loadDetail} />}
            {!detail && !detailError && <Loading label={t("approval.loadingDetail")} />}
            {detail && (
              <div className="space-y-3 p-1">
                <div className="grid gap-3 md:grid-cols-2">
                  <div>
                    <div className="mb-1 text-[11px] uppercase text-muted-foreground">
                      {t("approval.slot")}
                    </div>
                    <div className="space-y-1">
                      {detail.slots.map((s, i) => {
                        const filled = detail.approvals.find((a) => a.slot === i)
                        return (
                          <div
                            key={i}
                            className="flex items-center justify-between gap-2 rounded border bg-card px-2 py-1 text-xs"
                          >
                            <span>
                              {i + 1}. {s.label}{" "}
                              <span className="text-muted-foreground">
                                ({s.roles.join("/")})
                              </span>
                            </span>
                            {filled ? (
                              <span className="shrink-0 text-green-700">
                                {filled.actor}
                                {filled.self && ` · ${t("approval.selfReview")}`}
                              </span>
                            ) : (
                              <span className="shrink-0 text-amber-700">
                                {t("approval.slotPending")}
                              </span>
                            )}
                          </div>
                        )
                      })}
                    </div>
                    <p className="mt-1 text-[11px] text-muted-foreground">
                      {t("approval.submitterInfo", {
                        submitter: detail.submitter,
                        type: submitterLabel(t, detail.submitterType),
                        actor: actor.id,
                        actorType: actor.type,
                      })}
                      {detail.selfApproval === false &&
                        ` · ${t("approval.noSelfApproval")}`}
                    </p>
                  </div>

                  <div className="space-y-2">
                    {(pending || canPublish) && (
                      <>
                        {pending && (
                          <div className="flex gap-2">
                            <select
                              value={role}
                              onChange={(e) => setRole(e.target.value)}
                              className="rounded border border-input bg-card px-2 py-1 text-xs"
                              aria-label={t("approval.roleAria")}
                            >
                              {unfilledRoles(detail).map(([slot, r]) => (
                                <option key={`${slot}-${r}`} value={r}>
                                  {t("approval.slotOption", { slot: slot + 1, role: r })}
                                </option>
                              ))}
                            </select>
                            <input
                              value={comment}
                              onChange={(e) => setComment(e.target.value)}
                              placeholder={t("approval.commentPlaceholder")}
                              className="min-w-0 flex-1 rounded border border-input bg-card px-2 py-1 text-xs"
                            />
                            <Button size="sm" disabled={busy} onClick={() => act("approve")}>
                              {t("approval.approve")}
                            </Button>
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={busy}
                              onClick={() => act("reject")}
                            >
                              {t("approval.reject")}
                            </Button>
                          </div>
                        )}
                        {canPublish && (
                          <div className="rounded border border-green-300 bg-green-50 p-2 text-xs text-green-800">
                            {t("approval.readyToPublish")}
                          </div>
                        )}
                      </>
                    )}
                    {actionError && <ErrorBox error={actionError} />}
                    {flash && (
                      <div className="rounded border border-green-300 bg-green-50 px-2 py-1 text-xs text-green-800">
                        {flash}
                      </div>
                    )}
                    {detail.approvals.length > 0 && (
                      <div className="text-[11px] text-muted-foreground">
                        {t("approval.records", {
                          records: detail.approvals
                            .map(
                              (a) =>
                                `${a.actor}(${a.role})${a.comment ? `「${a.comment}」` : ""}`
                            )
                            .join("; "),
                        })}
                      </div>
                    )}
                    {detail.reason && (
                      <p className="text-[11px] text-muted-foreground">
                        {t("approval.reason", { reason: detail.reason })}
                      </p>
                    )}
                  </div>
                </div>
              </div>
            )}
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

function SlaCell({ sla }: { sla?: ChangeSummary["sla"] }) {
  const { t } = useTranslation()
  if (!sla) return <span className="text-muted-foreground">—</span>
  if (!sla.applicable) {
    return (
      <span
        className="text-muted-foreground"
        title={t("approval.sla.notApplicableTitle")}
      >
        {t("approval.sla.notApplicable")}
      </span>
    )
  }
  const cls = sla.overdue
    ? "text-red-600 font-medium"
    : sla.escalated
    ? "text-amber-700"
    : ""
  return (
    <span
      className={cls}
      title={t("approval.sla.escalateAt", { hours: sla.escalateAfterHours })}
    >
      {t("approval.sla.target", { hours: sla.targetHours })}
      {sla.overdue && ` · ${t("approval.sla.overdue")}`}
      {!sla.overdue && sla.escalated && ` · ${t("approval.sla.escalated")}`}
      <div className="text-[10px] text-muted-foreground">
        {t("approval.sla.waited", { hours: sla.elapsedHours.toFixed(1) })}
      </div>
    </span>
  )
}

function unfilledRoles(detail: ChangeDetail): [number, string][] {
  const out: [number, string][] = []
  detail.slots.forEach((s, i) => {
    if (detail.approvals.some((a) => a.slot === i)) return
    for (const r of s.roles) out.push([i, r])
  })
  return out
}

/**
 * Timestamps stay numeric and unlocalized: the approval trail is compared against audit entries
 * and logs by string, and an Arabic-Indic rendering of the same instant would break that.
 */
function formatTime(iso?: string): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes()
  )}`
}
