import { useState } from "react"
import { useSearchParams } from "react-router-dom"
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

// Enums and reason-code prefixes registered in the contract. These two tables do not come from an
// endpoint - they are part of the DSL contract (contracts/01 §3.6 / §5), so changing them requires a
// contract change and they never move because of a single decision.
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

// A whitelist instead of using the query parameter directly: on a typo or a hand-edited URL an invalid
// value would hide all three panels, making the page look like "there is nothing" - worse than falling
// back to the first tab.
const TAB_KEYS = ["logs", "audit", "enums"]
type TabKey = "logs" | "audit" | "enums"

export default function Observability() {
  // The tab lives in the URL: a refresh does not jump back to the first tab, and you can send someone
  // the "audit trail" tab directly.
  const [params, setParams] = useSearchParams()
  const raw = params.get("tab")
  const tab = TAB_KEYS.includes(raw ?? "") ? (raw as TabKey) : "logs"
  const setTab = (next: string) =>
    setParams(next === "logs" ? {} : { tab: next }, { replace: true })

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

  const logTotal = logs.data?.list.total ?? logs.data?.list.items.length
  const auditTotal = audit.data?.total ?? audit.data?.items.length

  return (
    <div className="p-6 space-y-4">
      <div>
        <h1 className="text-lg font-semibold">观测与审计</h1>
        <p className="text-sm text-muted-foreground">
          三块东西，三种来源，别混着读：
          <span className="text-foreground">决策日志</span>是引擎每次决策的流水、
          <span className="text-foreground">审计留痕</span>是人做过的动作、
          <span className="text-foreground">契约注册表</span>是静态约定（不随运行变化）。
        </p>
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger active={tab === "logs"} onClick={() => setTab("logs")}>
            决策日志
            <Count n={logTotal} failed={!!logs.error} />
          </TabsTrigger>
          <TabsTrigger active={tab === "audit"} onClick={() => setTab("audit")}>
            审计留痕
            <Count n={auditTotal} failed={!!audit.error} />
          </TabsTrigger>
          <TabsTrigger active={tab === "enums"} onClick={() => setTab("enums")}>
            契约注册表
            <Count n={decisionEnums.length + reasonCodes.length} />
          </TabsTrigger>
        </TabsList>

        <TabsContent value="logs">
          <SectionIntro
            source="引擎 → 平台推送的结构化日志（脱敏视图）"
            question="这次决策为什么是这个结果？"
            note="机器产出，一条对应一次求值。可逐条回放：拿当时的快照重算，比对结论与理由码是否仍然一致。"
          />
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
          <SectionIntro
            source="平台自身记录的操作事件（审批票、发布、密钥轮换）"
            question="谁在什么时候改动过什么？"
            note="人或代操作者执行动作的 agent 产生，一条对应一次状态迁移。决策日志里的记录不会出现在这里，反之亦然。"
          />
          <Async state={audit} loadingLabel="读取审计留痕">
            {(data) => {
              const actions = Array.from(new Set(data.items.map((a) => a.action)))
              return (
                <div className="space-y-3">
                  <div className="text-xs text-muted-foreground">
                    本页 {data.items.length} 条
                    {data.total !== undefined && data.total > data.items.length && (
                      <> · 累计 {data.total} 条</>
                    )}
                    {actions.length > 0 && (
                      <> · 涉及动作：{actions.join("、")}</>
                    )}
                  </div>
                  {data.items.length === 0 ? (
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
                              {a.from || a.to
                                ? `${a.from ?? "?"} → ${a.to ?? "?"}`
                                : "—"}
                            </TableCell>
                            <TableCell className="max-w-[26rem] text-xs text-muted-foreground">
                              {a.detail ?? a.reason ?? "—"}
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </div>
              )
            }}
          </Async>
        </TabsContent>

        <TabsContent value="enums">
          <SectionIntro
            source="契约文档 contracts/01 §3.6 / §5（编译进代码，不来自接口）"
            question="决策值和理由码允许取哪些值？"
            note="静态表，改它等于改 DSL 契约，要走契约变更。它不会因为某次决策而变，也不参与回放。"
          />
          <div className="space-y-5">
            <div className="space-y-2">
              <div>
                <h3 className="text-sm font-medium">
                  决策值
                  <span className="ml-1.5 text-xs font-normal text-muted-foreground">
                    共 {decisionEnums.length} 个
                  </span>
                </h3>
                <p className="text-xs text-muted-foreground">
                  决定这次求值的结论落在哪个分支
                </p>
              </div>
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
            </div>
            <div className="space-y-2">
              <div>
                <h3 className="text-sm font-medium">
                  理由码前缀
                  <span className="ml-1.5 text-xs font-normal text-muted-foreground">
                    共 {reasonCodes.length} 个
                  </span>
                </h3>
                <p className="text-xs text-muted-foreground">
                  说明为什么得到这个结论；日志里的理由码以此开头
                </p>
              </div>
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
          </div>
        </TabsContent>
      </Tabs>
    </div>
  )
}

function Count({ n, failed }: { n?: number; failed?: boolean }) {
  // When the fetch fails this panel is hidden and the error box is never seen - put a red dot on the tab,
  // otherwise "no data" and "cannot read" look identical in the navigation bar.
  if (failed) {
    return (
      <span
        title="这一栏取数失败"
        className="ml-1.5 rounded-full bg-red-100 px-1.5 text-[10px] font-normal text-red-700"
      >
        !
      </span>
    )
  }
  if (n === undefined) return null
  return (
    <span className="ml-1.5 rounded-full border px-1.5 text-[10px] font-normal tabular-nums text-muted-foreground">
      {n}
    </span>
  )
}

// One line at the top of each section: "what this answers / where the data comes from / how to read it".
// All three blocks look like tables, and without explicit boundaries only the color differs, so readers
// end up mixing human actions with machine decisions.
function SectionIntro({
  question,
  source,
  note,
}: {
  question: string
  source: string
  note: string
}) {
  return (
    <div className="mb-3 rounded-lg border bg-muted/40 px-3 py-2">
      <div className="text-sm font-medium">{question}</div>
      <div className="mt-1 space-y-0.5 text-xs text-muted-foreground">
        <div>
          <span className="text-foreground/70">来源</span> · {source}
        </div>
        <div>
          <span className="text-foreground/70">怎么读</span> · {note}
        </div>
      </div>
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
