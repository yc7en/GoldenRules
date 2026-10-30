import { useState } from "react"
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

const statusConfig: Record<string, { label: string; cls: string }> = {
  draft: { label: "草稿", cls: "bg-muted text-muted-foreground" },
  submitted: { label: "已提交", cls: "bg-blue-100 text-blue-700" },
  approval: { label: "审批中", cls: "bg-amber-100 text-amber-700" },
  approved: { label: "已批准", cls: "bg-blue-100 text-blue-700" },
  rejected: { label: "已驳回", cls: "bg-red-100 text-red-700" },
  published: { label: "已发布", cls: "bg-green-100 text-green-700" },
}

export default function Approval() {
  const changes = useAsync(() => api.changes.list(), [])
  const matrix = useAsync(() => api.changes.matrix(), [])

  return (
    <div className="p-6 space-y-4">
      <div>
        <h1 className="text-lg font-semibold">审批与发布</h1>
        <p className="text-sm text-muted-foreground">
          二维审批矩阵：提交者类型 × 风险等级。批准与发布都是真动作，会写审计。
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">审批矩阵（等级 × 提交者）</CardTitle>
        </CardHeader>
        <CardContent>
          <Async state={matrix} loadingLabel="读取审批矩阵">
            {(m) => <MatrixTable cells={m.cells} />}
          </Async>
          <p className="mt-2 text-xs text-red-600">
            硬规则：agent 只能提草稿，永远不能发布；L4 规则 agent 不得直接提交。
          </p>
        </CardContent>
      </Card>

      <Async state={changes} loadingLabel="读取变更单">
        {(data) =>
          data.items.length === 0 ? (
            <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
              还没有变更单。变更单由提交流程创建 —— 本页负责审批与发布，不负责建单。
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>变更单</TableHead>
                  <TableHead>包 / 版本</TableHead>
                  <TableHead>等级</TableHead>
                  <TableHead>提交者</TableHead>
                  <TableHead>审批进度</TableHead>
                  <TableHead>时效</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead className="text-right">操作</TableHead>
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
  const levels: RiskLevel[] = ["L1", "L2", "L3", "L4"]
  const submitters = [...new Set(cells.map((c) => c.submitterType))]
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>等级</TableHead>
          {submitters.map((s) => (
            <TableHead key={s}>{submitterLabel(s)}</TableHead>
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
                      禁止提交
                      {cell.forbiddenCause && (
                        <span className="ml-1 font-normal text-red-500">
                          （{cell.forbiddenCause}）
                        </span>
                      )}
                    </span>
                  ) : (
                    <span>
                      {cell.requiredCount} 人 · {cell.slotLabels.join(" / ")}
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
        setFlash(`已批准（角色 ${role}），剩余审批位 ${r.remaining}`)
      } else if (kind === "reject") {
        await api.changes.reject(change.changeId, role, comment)
        setFlash("已驳回")
      } else {
        const r = await api.packages.publish(
          change.packageId,
          change.packageVersion,
          change.changeId
        )
        setFlash(
          `已发布 ${r.version.packageId} ${r.version.packageVersion}（${r.version.status}）`
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
          <div className="text-[10px] font-normal text-muted-foreground">
            {formatTime(change.createdAt)}
          </div>
        </TableCell>
        <TableCell className="text-xs">
          {change.packageId}
          <span className="ml-1 text-muted-foreground">{change.packageVersion}</span>
          <div className="text-[10px] text-muted-foreground">{change.changeType}</div>
        </TableCell>
        <TableCell>
          <Badge className={`risk-badge-${change.riskLevel}`}>{change.riskLevel}</Badge>
        </TableCell>
        <TableCell className="text-xs">
          {change.submitter}
          <div className="text-[10px] text-muted-foreground">
            {change.submitterType}
          </div>
        </TableCell>
        <TableCell className="text-xs">
          {change.approved} / {change.required}
          {change.remaining > 0 && (
            <span className="ml-1 text-amber-700">差 {change.remaining}</span>
          )}
        </TableCell>
        <TableCell className="text-xs">
          <SlaCell sla={change.sla} />
        </TableCell>
        <TableCell>
          <Badge className={statusConfig[change.status]?.cls ?? ""}>
            {statusConfig[change.status]?.label ?? change.status}
          </Badge>
        </TableCell>
        <TableCell className="text-right">
          <div className="flex justify-end gap-1">
            <Button size="sm" variant="outline" onClick={toggle}>
              {open ? "收起" : "详情"}
            </Button>
            {canPublish && (
              <Button size="sm" disabled={busy} onClick={() => act("publish")}>
                发布
              </Button>
            )}
          </div>
        </TableCell>
      </TableRow>

      {open && (
        <TableRow>
          <TableCell colSpan={8} className="bg-muted/40">
            {detailError && <ErrorBox error={detailError} onRetry={loadDetail} />}
            {!detail && !detailError && <Loading label="读取变更详情" />}
            {detail && (
              <div className="space-y-3 p-1">
                <div className="grid gap-3 md:grid-cols-2">
                  <div>
                    <div className="mb-1 text-[11px] uppercase text-muted-foreground">
                      审批位
                    </div>
                    <div className="space-y-1">
                      {detail.slots.map((s, i) => {
                        const filled = detail.approvals.find((a) => a.slot === i)
                        return (
                          <div
                            key={i}
                            className="flex items-center justify-between rounded border bg-card px-2 py-1 text-xs"
                          >
                            <span>
                              {i + 1}. {s.label}{" "}
                              <span className="text-muted-foreground">
                                （{s.roles.join("/")}）
                              </span>
                            </span>
                            {filled ? (
                              <span className="text-green-700">
                                {filled.actor}
                                {filled.self && " · 自审"}
                              </span>
                            ) : (
                              <span className="text-amber-700">待批</span>
                            )}
                          </div>
                        )
                      })}
                    </div>
                    <p className="mt-1 text-[11px] text-muted-foreground">
                      提交人 {detail.submitter}（{detail.submitterType}）· 当前操作者{" "}
                      {actor.id}（{actor.type}）
                      {detail.selfApproval === false && " · 本等级不允许自审"}
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
                              aria-label="审批角色"
                            >
                              {unfilledRoles(detail).map(([slot, r]) => (
                                <option key={`${slot}-${r}`} value={r}>
                                  位 {slot + 1} · {r}
                                </option>
                              ))}
                            </select>
                            <input
                              value={comment}
                              onChange={(e) => setComment(e.target.value)}
                              placeholder="审批意见"
                              className="min-w-0 flex-1 rounded border border-input bg-card px-2 py-1 text-xs"
                            />
                            <Button size="sm" disabled={busy} onClick={() => act("approve")}>
                              批准
                            </Button>
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={busy}
                              onClick={() => act("reject")}
                            >
                              驳回
                            </Button>
                          </div>
                        )}
                        {canPublish && (
                          <div className="rounded border border-green-300 bg-green-50 p-2 text-xs text-green-800">
                            已批准，可以发布。发布后新包对该场景生效；在途请求仍用旧包跑完。
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
                        审批记录：
                        {detail.approvals
                          .map(
                            (a) =>
                              `${a.actor}(${a.role})${a.comment ? `「${a.comment}」` : ""}`
                          )
                          .join("；")}
                      </div>
                    )}
                    {detail.reason && (
                      <p className="text-[11px] text-muted-foreground">
                        变更理由：{detail.reason}
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
  if (!sla) return <span className="text-muted-foreground">—</span>
  if (!sla.applicable) {
    return (
      <span className="text-muted-foreground" title="已不在等审批，时效不再计算">
        不适用
      </span>
    )
  }
  const cls = sla.overdue
    ? "text-red-600 font-medium"
    : sla.escalated
    ? "text-amber-700"
    : ""
  return (
    <span className={cls} title={`升级阈值 ${sla.escalateAfterHours}h`}>
      目标 {sla.targetHours}h
      {sla.overdue && " · 已超期"}
      {!sla.overdue && sla.escalated && " · 已升级"}
      <div className="text-[10px] text-muted-foreground">
        已等 {sla.elapsedHours.toFixed(1)}h
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

function submitterLabel(s: string): string {
  const map: Record<string, string> = {
    engineer: "工程师",
    business: "业务",
    agent: "agent",
    risk: "风控",
    compliance: "合规",
    owner: "负责人",
  }
  return map[s] ?? s
}

function formatTime(iso?: string): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes()
  )}`
}
