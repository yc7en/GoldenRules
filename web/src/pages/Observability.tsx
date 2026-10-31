import { useState } from "react"
import { Trans, useTranslation } from "react-i18next"
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
// Only the labels are localized; the values themselves are contract and never translated.
const decisionEnums = [
  {
    value: "APPROVE",
    domainKey: "obs.enums.domain.approval",
    descKey: "obs.enums.desc.APPROVE",
  },
  {
    value: "REJECT",
    domainKey: "obs.enums.domain.approval",
    descKey: "obs.enums.desc.REJECT",
  },
  {
    value: "REVIEW",
    domainKey: "obs.enums.domain.approval",
    descKey: "obs.enums.desc.REVIEW",
  },
  {
    value: "ROUTE",
    domainKey: "obs.enums.domain.routing",
    descKey: "obs.enums.desc.ROUTE",
  },
] as const

const reasonCodes = [
  { code: "AMOUNT_", domainKey: "obs.enums.domain.limit", descKey: "obs.enums.desc.AMOUNT_" },
  { code: "FREQ_", domainKey: "obs.enums.domain.limit", descKey: "obs.enums.desc.FREQ_" },
  { code: "LIST_", domainKey: "obs.enums.domain.list", descKey: "obs.enums.desc.LIST_" },
  {
    code: "CONTRACT_",
    domainKey: "obs.enums.domain.system",
    descKey: "obs.enums.desc.CONTRACT_",
  },
  {
    code: "DEGRADE_",
    domainKey: "obs.enums.domain.system",
    descKey: "obs.enums.desc.DEGRADE_",
  },
  {
    code: "DEFAULT_",
    domainKey: "obs.enums.domain.system",
    descKey: "obs.enums.desc.DEFAULT_",
  },
] as const

// A whitelist instead of using the query parameter directly: on a typo or a hand-edited URL an invalid
// value would hide all three panels, making the page look like "there is nothing" - worse than falling
// back to the first tab.
const TAB_KEYS = ["logs", "audit", "enums"]
type TabKey = "logs" | "audit" | "enums"

export default function Observability() {
  const { t } = useTranslation()
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
              text: t("obs.logs.match", {
                decision: res.replay?.decision ?? "?",
                reasonCode: res.replay?.reasonCode ?? "?",
              }),
            }
          : {
              ok: false,
              text: res.reason
                ? t("obs.logs.mismatchReason", {
                    decision: res.original?.decision ?? "?",
                    reasonCode: res.original?.reasonCode ?? "?",
                    replayDecision: res.replay?.decision ?? "?",
                    replayReasonCode: res.replay?.reasonCode ?? "?",
                    reason: res.reason,
                  })
                : t("obs.logs.mismatch", {
                    decision: res.original?.decision ?? "?",
                    reasonCode: res.original?.reasonCode ?? "?",
                    replayDecision: res.replay?.decision ?? "?",
                    replayReasonCode: res.replay?.reasonCode ?? "?",
                  }),
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
        <h1 className="text-lg font-semibold">{t("obs.title")}</h1>
        <p className="text-sm text-muted-foreground">
          <Trans
            i18nKey="obs.subtitle"
            components={{ b: <span className="text-foreground" /> }}
          />
        </p>
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger active={tab === "logs"} onClick={() => setTab("logs")}>
            {t("obs.tab.logs")}
            <Count n={logTotal} failed={!!logs.error} />
          </TabsTrigger>
          <TabsTrigger active={tab === "audit"} onClick={() => setTab("audit")}>
            {t("obs.tab.audit")}
            <Count n={auditTotal} failed={!!audit.error} />
          </TabsTrigger>
          <TabsTrigger active={tab === "enums"} onClick={() => setTab("enums")}>
            {t("obs.tab.enums")}
            <Count n={decisionEnums.length + reasonCodes.length} />
          </TabsTrigger>
        </TabsList>

        <TabsContent value="logs">
          <SectionIntro
            source={t("obs.logs.source")}
            question={t("obs.logs.question")}
            note={t("obs.logs.note")}
          />
          <Async state={logs} loadingLabel={t("obs.logs.loading")}>
            {({ list, stats }) => (
              <div className="space-y-3">
                <div className="grid grid-cols-2 gap-3 md:grid-cols-6">
                  <Stat label={t("obs.logs.stat.total")} value={String(stats.total)} ltr />
                  <Stat
                    label={t("obs.logs.stat.byDecision")}
                    value={
                      Object.entries(stats.byDecision)
                        .map(([k, v]) => `${k} ${v}`)
                        .join(" · ") || "—"
                    }
                    ltr
                  />
                  <Stat
                    label={t("obs.logs.stat.degraded")}
                    value={String(stats.degraded)}
                    ltr
                  />
                  <Stat
                    label={t("obs.logs.stat.avgLatency")}
                    value={`${stats.avgLatencyMs} ms`}
                    ltr
                  />
                  <Stat
                    label={t("obs.logs.stat.maxLatency")}
                    value={`${stats.maxLatencyMs} ms`}
                    ltr
                  />
                  <Stat
                    label={t("obs.logs.stat.rawSnapshot")}
                    value={
                      stats.rawEnabled
                        ? t("obs.logs.stat.rawOn", { days: stats.retentionDays })
                        : t("obs.logs.stat.rawOff")
                    }
                  />
                </div>

                {replayError && <ErrorBox error={replayError} />}

                {list.items.length === 0 ? (
                  <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                    {t("obs.logs.empty")}
                  </div>
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("obs.logs.col.logId")}</TableHead>
                        <TableHead>{t("obs.logs.col.scene")}</TableHead>
                        <TableHead>{t("obs.logs.col.decision")}</TableHead>
                        <TableHead>{t("obs.logs.col.reasonCode")}</TableHead>
                        <TableHead>{t("obs.logs.col.hitRules")}</TableHead>
                        <TableHead>{t("obs.logs.col.degraded")}</TableHead>
                        <TableHead>{t("obs.logs.col.time")}</TableHead>
                        <TableHead className="text-end">{t("obs.logs.col.replay")}</TableHead>
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
                                <span className="text-red-600">{t("common.yes")}</span>
                              ) : (
                                t("common.no")
                              )}
                            </TableCell>
                            <TableCell className="text-xs" dir="ltr">
                              {formatMs(e.executionTimeMs)}
                            </TableCell>
                            <TableCell className="text-xs text-muted-foreground" dir="ltr">
                              {formatTime(e.executedAt)}
                            </TableCell>
                            <TableCell className="text-end">
                              <Button
                                size="sm"
                                variant="outline"
                                disabled={replaying === item.logId}
                                onClick={() => doReplay(item.logId)}
                              >
                                {replaying === item.logId
                                  ? t("obs.logs.replaying")
                                  : t("obs.logs.replay")}
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
            source={t("obs.audit.source")}
            question={t("obs.audit.question")}
            note={t("obs.audit.note")}
          />
          <Async state={audit} loadingLabel={t("obs.audit.loading")}>
            {(data) => {
              const actions = Array.from(new Set(data.items.map((a) => a.action)))
              return (
                <div className="space-y-3">
                  <div className="text-xs text-muted-foreground">
                    {t("obs.audit.count", { count: data.items.length })}
                    {data.total !== undefined && data.total > data.items.length && (
                      <> · {t("obs.audit.total", { count: data.total })}</>
                    )}
                    {actions.length > 0 && (
                      <> · {t("obs.audit.actions", { actions: actions.join(", ") })}</>
                    )}
                  </div>
                  {data.items.length === 0 ? (
                    <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
                      {t("obs.audit.empty")}
                    </div>
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>{t("obs.audit.col.time")}</TableHead>
                          <TableHead>{t("obs.audit.col.action")}</TableHead>
                          <TableHead>{t("obs.audit.col.actor")}</TableHead>
                          <TableHead>{t("obs.audit.col.object")}</TableHead>
                          <TableHead>{t("obs.audit.col.transition")}</TableHead>
                          <TableHead>{t("obs.audit.col.detail")}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {data.items.map((a) => (
                          <TableRow key={a.eventId}>
                            <TableCell
                              className="whitespace-nowrap text-xs text-muted-foreground"
                              dir="ltr"
                            >
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
                                <span className="ms-1 text-muted-foreground">
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
            source={t("obs.enums.source")}
            question={t("obs.enums.question")}
            note={t("obs.enums.note")}
          />
          <div className="space-y-5">
            <div className="space-y-2">
              <div>
                <h3 className="text-sm font-medium">
                  {t("obs.enums.decisionTitle")}
                  <span className="ms-1.5 text-xs font-normal text-muted-foreground">
                    {t("obs.enums.decisionCount", { count: decisionEnums.length })}
                  </span>
                </h3>
                <p className="text-xs text-muted-foreground">
                  {t("obs.enums.decisionHint")}
                </p>
              </div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("obs.enums.col.value")}</TableHead>
                    <TableHead>{t("obs.enums.col.domain")}</TableHead>
                    <TableHead>{t("obs.enums.col.description")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {decisionEnums.map((d) => (
                    <TableRow key={d.value}>
                      <TableCell className="font-medium">{d.value}</TableCell>
                      <TableCell>{t(d.domainKey)}</TableCell>
                      <TableCell className="text-muted-foreground">
                        {t(d.descKey)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            <div className="space-y-2">
              <div>
                <h3 className="text-sm font-medium">
                  {t("obs.enums.reasonTitle")}
                  <span className="ms-1.5 text-xs font-normal text-muted-foreground">
                    {t("obs.enums.reasonCount", { count: reasonCodes.length })}
                  </span>
                </h3>
                <p className="text-xs text-muted-foreground">
                  {t("obs.enums.reasonHint")}
                </p>
              </div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("obs.enums.col.prefix")}</TableHead>
                    <TableHead>{t("obs.enums.col.domain")}</TableHead>
                    <TableHead>{t("obs.enums.col.description")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {reasonCodes.map((r) => (
                    <TableRow key={r.code}>
                      <TableCell>
                        <code className="text-xs">{r.code}</code>
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline">{t(r.domainKey)}</Badge>
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {t(r.descKey)}
                      </TableCell>
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
  const { t } = useTranslation()
  // When the fetch fails this panel is hidden and the error box is never seen - put a red dot on the tab,
  // otherwise "no data" and "cannot read" look identical in the navigation bar.
  if (failed) {
    return (
      <span
        title={t("obs.tabFailed")}
        className="ms-1.5 rounded-full bg-red-100 px-1.5 text-[10px] font-normal text-red-700"
      >
        !
      </span>
    )
  }
  if (n === undefined) return null
  return (
    <span className="ms-1.5 rounded-full border px-1.5 text-[10px] font-normal tabular-nums text-muted-foreground">
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
  const { t } = useTranslation()
  return (
    <div className="mb-3 rounded-lg border bg-muted/40 px-3 py-2">
      <div className="text-sm font-medium">{question}</div>
      <div className="mt-1 space-y-0.5 text-xs text-muted-foreground">
        <div>
          <span className="text-foreground/70">{t("obs.introSource")}</span> · {source}
        </div>
        <div>
          <span className="text-foreground/70">{t("obs.introNote")}</span> · {note}
        </div>
      </div>
    </div>
  )
}

function Stat({
  label,
  value,
  ltr,
}: {
  label: string
  value: string
  ltr?: boolean
}) {
  return (
    <div className="rounded-lg border bg-card px-3 py-2">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      {/* Numbers are direction-weak: inside an RTL paragraph "0 ms" would render as "ms 0", so
          values that are entirely numeric or ASCII are pinned to LTR. */}
      <div className="mt-0.5 text-sm font-medium" dir={ltr ? "ltr" : undefined}>
        {value}
      </div>
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
  // Contract enum: the value is displayed verbatim in every locale.
  return <Badge className={cls}>{decision}</Badge>
}

function formatMs(ms?: number): string {
  if (ms === undefined) return "—"
  return ms < 1 ? `${(ms * 1000).toFixed(0)} µs` : `${ms.toFixed(2)} ms`
}

/**
 * Log timestamps stay `MM-DD HH:mm:ss` in every locale: entries are sorted and diffed as strings
 * across screenshots and exports, so a localized rendering would make two views incomparable.
 */
function formatTime(iso?: string): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes()
  )}:${pad(d.getSeconds())}`
}
