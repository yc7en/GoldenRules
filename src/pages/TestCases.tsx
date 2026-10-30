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
import { Async, ErrorBox } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api, ApiError } from "@/lib/api"
import type { RulesetDefinition, TestCaseRecord } from "@/lib/api-types"

const authorLabel: Record<string, string> = {
  agent: "AI 生成",
  engineer: "工程师",
  business: "业务",
}

const statusConfig: Record<
  TestCaseRecord["status"],
  { label: string; cls: string }
> = {
  pending_review: { label: "待确认", cls: "bg-amber-100 text-amber-700" },
  approved: { label: "已确认", cls: "bg-green-100 text-green-700" },
  rejected: { label: "已拒绝", cls: "bg-red-100 text-red-700" },
}

interface Row extends TestCaseRecord {
  rulesetId: string
  rulesetVersion: string
}

export default function TestCases() {
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
      // 用例状态存在规则集里，只能整份 PUT 回去 —— 后端会顺手再跑一遍 dry-run，
      // 所以这一步同时也在证明「这份规则集仍然编得过」。
      const fresh = await api.rulesets.get(rs.rulesetId)
      const tests = (fresh.tests ?? []).map((t) =>
        t.testId === testId ? { ...t, status } : t
      )
      const report = await api.rulesets.save(rs.rulesetId, { ...fresh, tests })
      if (!report.valid) {
        const first = report.errors[0]
        setActionError(
          new ApiError(
            422,
            first?.code ?? "VALIDATION_FAILED",
            `规则集保存被拒：${first?.message ?? "未知诊断"}（${first?.path ?? ""}）`,
            report
          )
        )
        return
      }
      setNotice(
        `${testId} → ${statusConfig[status].label}（规则集 ${rs.rulesetId} 已保存，规则版本未变）`
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
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">测试用例</h1>
          <p className="text-sm text-muted-foreground">
            业务人员主要确认测试用例；AI 生成的期望值由确认人负责
          </p>
        </div>
        <div className="flex gap-2">
          <Button
            variant={filter === "pending_review" ? "default" : "outline"}
            size="sm"
            onClick={() => setFilter("pending_review")}
          >
            待我确认
          </Button>
          <Button
            variant={filter === "all" ? "default" : "outline"}
            size="sm"
            onClick={() => setFilter("all")}
          >
            全部
          </Button>
        </div>
      </div>

      <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
        只有「已确认」的用例会被发布门禁执行。包内一条已确认用例都没有时，门禁会通过但不验任何东西
        —— 那是空跑，不是通过。
      </div>

      {actionError && <ErrorBox error={actionError} />}
      {notice && (
        <div className="rounded-md border border-green-300 bg-green-50 px-3 py-2 text-xs text-green-800">
          {notice}
        </div>
      )}

      <Async state={state} loadingLabel="读取用例">
        {(data) => {
          const rows = flatten(data.items)
          const filtered =
            filter === "all" ? rows : rows.filter((r) => r.status === filter)
          if (filtered.length === 0) {
            return (
              <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                {filter === "pending_review"
                  ? "没有待确认的用例。切到「全部」看已确认与已拒绝的。"
                  : "所有规则集里都没有用例。"}
              </div>
            )
          }
          return (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>用例 ID</TableHead>
                  <TableHead>规则集</TableHead>
                  <TableHead>场景</TableHead>
                  <TableHead>输入</TableHead>
                  <TableHead>期望</TableHead>
                  <TableHead>作者</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead className="text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((c) => {
                  const rs = data.items.find((r) => r.rulesetId === c.rulesetId)!
                  const key = `${c.rulesetId}/${c.testId}`
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
                          <code className="ml-1 text-xs">{c.expect.reasonCode}</code>
                        )}
                      </TableCell>
                      <TableCell>{authorLabel[c.author] ?? c.author}</TableCell>
                      <TableCell>
                        <Badge className={statusConfig[c.status].cls}>
                          {statusConfig[c.status].label}
                        </Badge>
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          {c.status !== "approved" && (
                            <Button
                              size="sm"
                              disabled={busy === key}
                              onClick={() => apply(rs, c.testId, "approved")}
                            >
                              {busy === key ? "保存中…" : "确认"}
                            </Button>
                          )}
                          {c.status !== "rejected" && (
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={busy === key}
                              onClick={() => apply(rs, c.testId, "rejected")}
                            >
                              拒绝
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
        // 用例自己没写 rulesetId 时，用宿主规则集补上——列表里必须说清它属于哪个规则集
        rulesetId: t.rulesetId ?? rs.rulesetId,
        rulesetVersion: rs.version,
      })
    }
  }
  return out
}
