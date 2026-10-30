import { useState } from "react"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import { Async, ErrorBox } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api, ApiError } from "@/lib/api"

// 契约里登记的枚举与理由码前缀。这两个表不来自接口 —— 它们是 DSL 契约的一部分
// （contracts/01 §3.6 / §5），改它要走契约变更，不会因为某次决策而变。
const decisionEnums = [
  { value: "APPROVE", domain: "审批域", desc: "通过/放行" },
  { value: "REJECT", domain: "审批域", desc: "拒绝" },
  { value: "REVIEW", domain: "审批域", desc: "转人工复核" },
  { value: "ROUTE", domain: "路由域", desc: "路由到指定通道" },
]

const reasonCodes = [
  { code: "AMOUNT_", domain: "LIMIT", desc: "金额相关（超限/复核）" },
  { code: "FREQ_", domain: "LIMIT", desc: "频次相关" },
  { code: "LIST_", domain: "LIST", desc: "名单命中" },
  { code: "CONTRACT_", domain: "SYSTEM", desc: "契约违规兜底" },
  { code: "DEGRADE_", domain: "SYSTEM", desc: "降级相关" },
  { code: "DEFAULT_", domain: "SYSTEM", desc: "无规则命中兜底" },
]

export default function Observability() {
  const [tab, setTab] = useState("logs")
  const logs = useAsync(async () => {
    const [list, stats] = await Promise.all([api.logs.list(50), api.logs.stats()])
    return { list, stats }
  }, [])
  const audit = useAsync(() => api.audit.list(50), [])
  const [replay, setReplay] = useState<
    Record<string, { ok: boolean; text: string }>
  >({})
  const [replaying, setReplaying] = useState<string | null>(null)
  const [replayError, setReplayError] = useState<ApiError | null>(null)

  const doReplay = async (logId: string) => {
    setReplaying(logId)
    setReplayError(null)
    try {
      const r = await api.logs.replay(logId)
      const res = r.result
      setReplay((prev) => ({
        ...prev,
        [logId]: res.match
          ? {
              ok: true,
              text: `一致：${res.replay?.decision ?? "?"} / ${
                res.replay?.reasonCode ?? "?"
              }`,
            }
          : {
              ok: false,
              text: `不一致：原 ${
                res.original?.decision ?? "?"
              } / ${res.original?.reasonCode ?? "?"}，回放 ${
                res.replay?.decision ?? "?"
              } / ${res.replay?.reasonCode ?? "?"}${
                res.reason ? `（${res.reason}）` : ""
              }`,
            },
      }))
    } catch (e) {
      setReplayError(e instanceof ApiError ? e : new ApiError(0, "UNKNOWN", String(e)))
    } finally {
      setReplaying(null)
    }
  }

  return (
    <div className="p-6 space-y-4">
      <div>
        <h1 className="text-lg font-semibold">观测与审计</h1>
        <p className="text-sm text-muted-foreground">
          决策日志（脱敏视图）、审计留痕、契约注册表
        </p>
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger active={tab === "logs"} onClick={() => setTab("logs")}>
            决策日志
          </TabsTrigger>
          <TabsTrigger active={tab === "audit"} onClick={() => setTab("audit")}>
            审计留痕
          </TabsTrigger>
          <TabsTrigger active={tab === "enums"} onClick={() => setTab("enums")}>
            契约注册表
          </TabsTrigger>
        </TabsList>

        <TabsContent value="logs">
          <Async state={logs} loadingLabel="读取决策日志">
            {({ list, stats }) => (
              <div className="space-y-3">
                <div className="grid grid-cols-2 gap-3 md:grid-cols-6">
                  <Stat label="总条数" value={String(stats.total)} />
                  <Stat
                    label="决策分布"
                    value={
                      Object.entries(stats.byDecision)
                        .map(([k, v]) => `${k} ${v}`)
                        .join(" · ") || "—"
                    }
                  />
                  <Stat label="降级" value={String(stats.degraded)} />
                  <Stat label="平均耗时" value={`${stats.avgLatencyMs} ms`} />
                  <Stat label="最大耗时" value={`${stats.maxLatencyMs} ms`} />
                  <Stat
                    label="原始快照"
                    value={stats.rawEnabled ? `保留 ${stats.retentionDays} 天` : "未开启"}
                  />
                </div>

                {replayError && <ErrorBox error={replayError} />}

                {list.items.length === 0 ? (
                  <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                    还没有决策日志。引擎每次决策会把结构化日志推到平台 ——
                    先跑一次试跑，或在引擎侧发一次决策。
                  </div>
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>日志 ID</TableHead>
                        <TableHead>场景</TableHead>
                        <TableHead>决策</TableHead>
                        <TableHead>理由码</TableHead>
                        <TableHead>命中规则</TableHead>
                        <TableHead>降级</TableHead>
                        <TableHead>耗时</TableHead>
                        <TableHead>时间</TableHead>
                        <TableHead className="text-right">回放</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {list.items.map((item) => {
                        const e = item.entry
                        const rp = replay[item.logId]
                        return (
                          <TableRow key={item.logId}>
                            <TableCell className="font-mono text-[11px]">
                              {item.logId}
                              <div className="text-[10px] text-muted-foreground">
                                {e.snapshotId}
                              </div>
                            </TableCell>
                            <TableCell>
                              <Badge variant="secondary">{e.scene}</Badge>
                              <div className="text-[10px] text-muted-foreground">
                                {e.rulesetId} {e.rulesetVersion}
                              </div>
                            </TableCell>
                            <TableCell>
                              <DecisionBadge decision={e.decision} />
                            </TableCell>
                            <TableCell>
                              <code className="text-xs">{e.reasonCode}</code>
                            </TableCell>
                            <TableCell className="text-xs">
                              {(e.hitRules ?? []).map((h) => h.ruleId).join(", ") || "—"}
                            </TableCell>
                            <TableCell>
                              {e.degraded ? (
                                <span className="text-red-600">是</span>
                              ) : (
                                "否"
                              )}
                            </TableCell>
                            <TableCell className="text-xs">
                              {formatMs(e.executionTimeMs)}
                            </TableCell>
                            <TableCell className="text-xs text-muted-foreground">
                              {formatTime(e.executedAt)}
                            </TableCell>
                            <TableCell className="text-right">
                              <Button
                                size="sm"
                                variant="outline"
                                disabled={replaying === item.logId}
                                onClick={() => doReplay(item.logId)}
                              >
                                {replaying === item.logId ? "回放中…" : "回放"}
                              </Button>
                              {rp && (
                                <div
                                  className={`mt-1 text-[10px] ${
                                    rp.ok ? "text-green-700" : "text-red-600"
                                  }`}
                                >
                                  {rp.text}
                                </div>
                              )}
                            </TableCell>
                          </TableRow>
                        )
                      })}
                    </TableBody>
                  </Table>
                )}
              </div>
            )}
          </Async>
        </TabsContent>

        <TabsContent value="audit">
          <Async state={audit} loadingLabel="读取审计留痕">
            {(data) =>
              data.items.length === 0 ? (
                <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                  还没有审计事件。审批、发布、密钥轮换都会在这里留痕。
                </div>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>时间</TableHead>
                      <TableHead>动作</TableHead>
                      <TableHead>操作者</TableHead>
                      <TableHead>对象</TableHead>
                      <TableHead>状态迁移</TableHead>
                      <TableHead>说明</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {data.items.map((a) => (
                      <TableRow key={a.eventId}>
                        <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                          {formatTime(a.at)}
                        </TableCell>
                        <TableCell>
                          <code className="text-xs">{a.action}</code>
                        </TableCell>
                        <TableCell className="text-xs">
                          {a.actor}
                          <div className="text-[10px] text-muted-foreground">
                            {a.actorType}
                          </div>
                        </TableCell>
                        <TableCell className="text-xs">
                          {a.changeId ?? a.packageId ?? "—"}
                          {a.packageVersion && (
                            <span className="ml-1 text-muted-foreground">
                              {a.packageVersion}
                            </span>
                          )}
                        </TableCell>
                        <TableCell className="text-xs">
                          {a.from || a.to ? `${a.from ?? "?"} → ${a.to ?? "?"}` : "—"}
                        </TableCell>
                        <TableCell className="max-w-[26rem] text-xs text-muted-foreground">
                          {a.detail ?? a.reason ?? "—"}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )
            }
          </Async>
        </TabsContent>

        <TabsContent value="enums">
          <div className="space-y-4">
            <p className="text-xs text-muted-foreground">
              这两张表来自契约（contracts/01 §3.6 / §5），不是运行时接口返回 ——
              改它们等于改 DSL 契约。
            </p>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>决策值</TableHead>
                  <TableHead>域</TableHead>
                  <TableHead>说明</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {decisionEnums.map((d) => (
                  <TableRow key={d.value}>
                    <TableCell className="font-medium">{d.value}</TableCell>
                    <TableCell>{d.domain}</TableCell>
                    <TableCell className="text-muted-foreground">{d.desc}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>理由码前缀</TableHead>
                  <TableHead>域</TableHead>
                  <TableHead>说明</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {reasonCodes.map((r) => (
                  <TableRow key={r.code}>
                    <TableCell>
                      <code className="text-xs">{r.code}</code>
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline">{r.domain}</Badge>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{r.desc}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </TabsContent>
      </Tabs>
    </div>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border bg-card px-3 py-2">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-sm font-medium">{value}</div>
    </div>
  )
}

function DecisionBadge({ decision }: { decision: string }) {
  const cls =
    decision === "REJECT"
      ? "bg-red-100 text-red-700"
      : decision === "APPROVE"
      ? "bg-green-100 text-green-700"
      : "bg-amber-100 text-amber-700"
  return <Badge className={cls}>{decision}</Badge>
}

function formatMs(ms?: number): string {
  if (ms === undefined) return "—"
  return ms < 1 ? `${(ms * 1000).toFixed(0)} µs` : `${ms.toFixed(2)} ms`
}

function formatTime(iso?: string): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes()
  )}:${pad(d.getSeconds())}`
}
