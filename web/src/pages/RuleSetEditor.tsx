import { useMemo, useState } from "react"
import { Trans, useTranslation } from "react-i18next"
import { useParams, Link } from "react-router-dom"
import Editor from "@monaco-editor/react"
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
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import { Async, ErrorBox } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api, ApiError, getActor } from "@/lib/api"
import type {
  RulesetDefinition,
  TrialResult,
  ValidationIssue,
  ValidationReport,
} from "@/lib/api-types"
import { cn } from "@/lib/utils"
import {
  Play,
  CheckCircle2,
  XCircle,
  AlertTriangle,
  ArrowLeft,
  Save,
  Info,
} from "lucide-react"

export default function RuleSetEditor() {
  const { t } = useTranslation()
  const { id } = useParams()
  const ruleset = useAsync(
    () =>
      id
        ? api.rulesets.get(id)
        : Promise.reject(new ApiError(400, "MISSING_ID", t("editor.missingId"))),
    [id]
  )
  const facts = useAsync(() => api.facts.list(), [])

  return (
    <div className="p-6">
      <Async state={ruleset} loadingLabel={t("editor.loading")}>
        {(def) => (
          <EditorInner
            // Reset the editor content when revision changes, otherwise the text is still stale after a successful save
            key={`${def.rulesetId}-${def.revision ?? 0}`}
            ruleset={def}
            catalogVersion={facts.data?.catalogVersion}
            onSaved={ruleset.reload}
          />
        )}
      </Async>
    </div>
  )
}

function EditorInner({
  ruleset,
  catalogVersion,
  onSaved,
}: {
  ruleset: RulesetDefinition
  catalogVersion?: string
  onSaved: () => void
}) {
  const { t } = useTranslation()
  const initialText = useMemo(
    () => JSON.stringify(ruleset, null, 2),
    [ruleset]
  )
  const [text, setText] = useState(initialText)
  const [view, setView] = useState<"structured" | "expression" | "table">(
    ruleset.table ? "table" : "structured"
  )

  const parsed = useMemo(
    () => parseDef(text, t("editor.topNotObject")),
    [text, t]
  )
  const dirty = text !== initialText

  const [report, setReport] = useState<ValidationReport | null>(null)
  const [busy, setBusy] = useState<"validate" | "save" | "trial" | null>(null)
  const [actionError, setActionError] = useState<ApiError | null>(null)
  const [flash, setFlash] = useState<string | null>(null)

  const [trialInput, setTrialInput] = useState(() =>
    JSON.stringify(ruleset.tests?.[0]?.input ?? {}, null, 2)
  )
  const [trial, setTrial] = useState<TrialResult | null>(null)
  const [trialError, setTrialError] = useState<ApiError | null>(null)

  const guard = (): RulesetDefinition | null => {
    if (!parsed.ok) {
      setActionError(
        new ApiError(
          400,
          "JSON_PARSE",
          t("editor.jsonParse", { message: parsed.message })
        )
      )
      return null
    }
    setActionError(null)
    return parsed.value
  }

  const handleValidate = async () => {
    const def = guard()
    if (!def) return
    setBusy("validate")
    setFlash(null)
    try {
      setReport(await api.rulesets.validate(ruleset.rulesetId, def))
    } catch (e) {
      setActionError(toErr(e))
    } finally {
      setBusy(null)
    }
  }

  const handleSave = async () => {
    const def = guard()
    if (!def) return
    setBusy("save")
    setFlash(null)
    try {
      const r = await api.rulesets.save(ruleset.rulesetId, def)
      setReport(r)
      if (r.valid) {
        setFlash(t("editor.saved", { from: ruleset.revision ?? "?" }))
        onSaved()
      } else {
        setActionError(
          new ApiError(
            422,
            r.errors[0]?.code ?? "VALIDATION_FAILED",
            t("editor.saveRejected", { message: r.errors[0]?.message ?? "—" }),
            r
          )
        )
      }
    } catch (e) {
      setActionError(toErr(e))
    } finally {
      setBusy(null)
    }
  }

  const handleTrial = async () => {
    setBusy("trial")
    setTrialError(null)
    try {
      const factsInput = JSON.parse(trialInput) as Record<string, unknown>
      if (typeof factsInput !== "object" || factsInput === null || Array.isArray(factsInput)) {
        throw new Error(t("editor.trialInputMustBeObject"))
      }
      const actor = getActor()
      const now = new Date().toISOString().replace(/\.\d+Z$/, "Z")
      const result = await api.engine.trial({
        snapshot: {
          snapshotId: `trial-${Date.now()}`,
          tenantId: "pay-cn",
          scene: ruleset.scene,
          factSchemaVersion: catalogVersion ?? "",
          facts: factsInput,
        },
        context: {
          tenantId: "pay-cn",
          scene: ruleset.scene,
          requestTime: now,
          traceId: `trace-ui-${Date.now()}`,
          rulesetId: ruleset.rulesetId,
          actor: { type: actor.type, id: actor.id },
        },
      })
      setTrial(result)
    } catch (e) {
      if (e instanceof SyntaxError) {
        setTrialError(
          new ApiError(
            400,
            "JSON_PARSE",
            t("editor.trialInputNotJson", { message: e.message })
          )
        )
      } else {
        setTrialError(toErr(e))
      }
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <Link to="/rulesets">
            <Button variant="ghost" size="icon">
              <ArrowLeft className="size-4 rtl:rotate-180" />
            </Button>
          </Link>
          <div className="min-w-0">
            <h1 className="flex flex-wrap items-center gap-2 text-lg font-semibold">
              {ruleset.rulesetId}
              <Badge variant="secondary">{ruleset.scene}</Badge>
              {dirty && (
                <Badge className="bg-amber-100 text-amber-700">
                  {t("editor.unsaved")}
                </Badge>
              )}
            </h1>
            <p className="text-sm text-muted-foreground">
              {t("editor.meta", {
                version: ruleset.version,
                hitStrategy: ruleset.hitStrategy,
                defaultDecision: ruleset.defaultDecision,
                revision: ruleset.revision ?? 0,
              })}
              {ruleset.updatedBy &&
                t("editor.lastBy", { actor: ruleset.updatedBy })}
            </p>
          </div>
        </div>
        <div className="flex shrink-0 gap-2">
          <Button variant="outline" disabled={busy !== null} onClick={handleValidate}>
            {busy === "validate" ? t("editor.validating") : t("editor.validate")}
          </Button>
          <Button variant="outline" disabled={busy !== null} onClick={handleTrial}>
            <Play className="size-4" />
            {busy === "trial" ? t("editor.trialing") : t("editor.trial")}
          </Button>
          <Button disabled={busy !== null || !dirty} onClick={handleSave}>
            <Save className="size-4" />
            {busy === "save" ? t("editor.saving") : t("editor.save")}
          </Button>
        </div>
      </div>

      {actionError && <ErrorBox error={actionError} />}
      {flash && (
        <div className="rounded-md border border-green-300 bg-green-50 px-3 py-2 text-xs text-green-800">
          {flash}
        </div>
      )}

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <div className="space-y-4 lg:col-span-2">
          <Tabs value={view} onValueChange={(v) => setView(v as typeof view)}>
            <TabsList>
              <TabsTrigger
                active={view === "structured"}
                onClick={() => setView("structured")}
              >
                {t("editor.tab.structured")}
              </TabsTrigger>
              <TabsTrigger
                active={view === "expression"}
                onClick={() => setView("expression")}
              >
                {t("editor.tab.expression")}
              </TabsTrigger>
              <TabsTrigger active={view === "table"} onClick={() => setView("table")}>
                {t("editor.tab.table")}
              </TabsTrigger>
            </TabsList>

            <TabsContent value="structured">
              <Card>
                <CardHeader>
                  <CardTitle className="flex items-center justify-between gap-2 text-sm">
                    {t("editor.defTitle")}
                    <span className="text-[11px] font-normal text-muted-foreground">
                      {t("editor.defHint")}
                    </span>
                  </CardTitle>
                </CardHeader>
                <CardContent>
                  <div className="h-[460px]">
                    <Editor
                      height="460px"
                      defaultLanguage="json"
                      theme="vs-dark"
                      value={text}
                      onChange={(v) => setText(v ?? "")}
                      options={{ minimap: { enabled: false }, tabSize: 2 }}
                    />
                  </div>
                  {!parsed.ok && (
                    <p className="mt-2 text-xs text-red-600">
                      {t("editor.parseFailed", { message: parsed.message })}
                    </p>
                  )}
                </CardContent>
              </Card>
            </TabsContent>

            <TabsContent value="expression">
              <ExpressionView ruleset={ruleset} />
            </TabsContent>

            <TabsContent value="table">
              <TableView ruleset={ruleset} />
            </TabsContent>
          </Tabs>
        </div>

        <div className="space-y-4">
          <ValidationPanel result={report} dirty={dirty} />
          <TrialPanel
            input={trialInput}
            setInput={setTrialInput}
            result={trial}
            error={trialError}
            scene={ruleset.scene}
            rulesetId={ruleset.rulesetId}
          />
        </div>
      </div>
    </div>
  )
}

function ExpressionView({ ruleset }: { ruleset: RulesetDefinition }) {
  const { t } = useTranslation()
  const rules = ruleset.rules ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">{t("editor.exprTitle")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {rules.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("editor.exprNote")}</p>
        ) : (
          rules.map((r) => (
            <div key={r.ruleId} className="rounded-md border p-3">
              <div className="mb-2 flex items-center justify-between gap-2">
                <span className="text-sm font-medium">
                  {r.ruleId}
                  <span className="ms-2 text-[11px] font-normal text-muted-foreground">
                    {r.version} · {t("editor.priority", { priority: r.priority })}
                  </span>
                </span>
                <Badge className={cn(`risk-badge-${r.riskLevel}`)}>{r.riskLevel}</Badge>
              </div>
              <div className="grid grid-cols-[72px_1fr] gap-2 text-xs">
                <span className="text-muted-foreground">WHEN</span>
                <code className="rounded bg-muted px-2 py-1">{exprToText(r.when)}</code>
                <span className="text-muted-foreground">THEN</span>
                <code className="rounded bg-muted px-2 py-1">
                  {r.then.decision} · {r.then.reasonCode}
                </code>
                {r.then.assign && r.then.assign.length > 0 && (
                  <>
                    <span className="text-muted-foreground">ASSIGN</span>
                    <code className="rounded bg-muted px-2 py-1">
                      {r.then.assign
                        .map((a) => `${a.var} = ${exprToText(a.expr)}`)
                        .join("; ")}
                    </code>
                  </>
                )}
              </div>
            </div>
          ))
        )}
      </CardContent>
    </Card>
  )
}

function TableView({ ruleset }: { ruleset: RulesetDefinition }) {
  const { t } = useTranslation()
  const dt = ruleset.table
  if (!dt) {
    return (
      <Card>
        <CardContent className="p-6">
          <p className="text-sm text-muted-foreground">{t("editor.tableNone")}</p>
        </CardContent>
      </Card>
    )
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">
          {t("editor.tableTitle", { tableId: dt.tableId })}
        </CardTitle>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-12">#</TableHead>
              {dt.columns.map((c) => (
                <TableHead key={c.fact}>{c.fact}</TableHead>
              ))}
              <TableHead>{t("editor.decision")}</TableHead>
              <TableHead>{t("editor.reasonCode")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {dt.rows.map((row, i) => (
              <TableRow key={i}>
                <TableCell>{i + 1}</TableCell>
                {dt.columns.map((c) => (
                  <TableCell key={c.fact} className="font-mono text-xs">
                    {renderCell(row.conditions[c.fact])}
                  </TableCell>
                ))}
                <TableCell>
                  <Badge variant="outline">{row.then.decision}</Badge>
                </TableCell>
                <TableCell>
                  <code className="text-xs">{row.then.reasonCode}</code>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {dt.defaultRow && (
          <p className="mt-3 text-xs text-muted-foreground">
            {t("editor.tableDefault", {
              decision: dt.defaultRow.decision,
              reasonCode: dt.defaultRow.reasonCode,
            })}
          </p>
        )}
      </CardContent>
    </Card>
  )
}

function ValidationPanel({
  result,
  dirty,
}: {
  result: ValidationReport | null
  dirty: boolean
}) {
  const { t } = useTranslation()
  if (!result) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">{t("editor.validationTitle")}</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">{t("editor.validationIdle")}</p>
        </CardContent>
      </Card>
    )
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2 text-sm">
          {t("editor.validationTitle")}
          {result.valid ? (
            <Badge className="bg-green-100 text-green-700">
              <CheckCircle2 className="size-3" /> {t("editor.passed")}
            </Badge>
          ) : (
            <Badge className="bg-red-100 text-red-700">
              <XCircle className="size-3" />{" "}
              {t("editor.errors", { count: result.errors.length })}
            </Badge>
          )}
          {dirty && (
            <span className="text-[11px] font-normal text-amber-700">
              {t("editor.validationEdited")}
            </span>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2">
        {result.stats && (
          <p className="text-[11px] text-muted-foreground">
            {t("editor.validationStats", {
              rules: result.stats.rules,
              slots: result.stats.slots,
              depth: result.stats.maxDepth,
              nodes: result.stats.maxNodes,
              assign: result.stats.assignCount,
            })}
          </p>
        )}
        {result.errors.map((e, i) => (
          <IssueBox key={`e${i}`} issue={e} />
        ))}
        {result.warnings.map((w, i) => (
          <IssueBox key={`w${i}`} issue={w} />
        ))}
        {result.errors.length === 0 && result.warnings.length === 0 && (
          <p className="text-sm text-green-700">{t("editor.validationOk")}</p>
        )}
      </CardContent>
    </Card>
  )
}

function IssueBox({ issue }: { issue: ValidationIssue }) {
  const { t } = useTranslation()
  const warn = issue.level === "warn"
  return (
    <div
      className={cn(
        "rounded-md border p-2 text-xs",
        warn ? "border-amber-200 bg-amber-50" : "border-red-200 bg-red-50"
      )}
    >
      <div
        className={cn(
          "flex items-center gap-1 font-medium",
          warn ? "text-amber-700" : "text-red-700"
        )}
      >
        {warn ? <AlertTriangle className="size-3" /> : <XCircle className="size-3" />}
        {issue.code}
        {issue.ruleId && <span className="font-normal">· {issue.ruleId}</span>}
      </div>
      <p className={warn ? "text-amber-800" : "text-red-800"}>{issue.message}</p>
      <p className={cn("font-mono", warn ? "text-amber-600" : "text-red-600")}>
        {issue.path}
      </p>
      {issue.suggest && (
        <p className={warn ? "text-amber-600" : "text-red-500"}>
          {t("editor.suggest", { text: issue.suggest })}
        </p>
      )}
    </div>
  )
}

function TrialPanel({
  input,
  setInput,
  result,
  error,
  scene,
  rulesetId,
}: {
  input: string
  setInput: (v: string) => void
  result: TrialResult | null
  error: ApiError | null
  scene: string
  rulesetId: string
}) {
  const { t } = useTranslation()
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center justify-between gap-2 text-sm">
          {t("editor.trialTitle")}
          <span className="text-[11px] font-normal text-muted-foreground">
            {t("editor.trialVia")}
          </span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-start gap-1 rounded border border-blue-200 bg-blue-50 px-2 py-1 text-[11px] text-blue-800">
          <Info className="mt-0.5 size-3 shrink-0" />
          <span>
            <Trans
              i18nKey="editor.trialHint"
              components={{ b: <strong className="font-semibold" /> }}
            />
          </span>
        </div>
        <label className="block text-[11px] text-muted-foreground">
          {t("editor.trialFactsLabel", { scene, rulesetId })}
        </label>
        <textarea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          className="h-36 w-full rounded-md border border-input bg-transparent p-2 font-mono text-xs"
          placeholder='{ "request.amount": "1500" }'
        />
        {error && <ErrorBox error={error} />}
        {result && (
          <div className="space-y-2">
            <div className="flex flex-wrap items-center gap-2">
              <Badge
                className={
                  result.decision === "REJECT"
                    ? "bg-red-100 text-red-700"
                    : result.decision === "APPROVE"
                    ? "bg-green-100 text-green-700"
                    : "bg-amber-100 text-amber-700"
                }
              >
                {t("editor.trialDecision", { decision: result.decision })}
              </Badge>
              <code className="text-xs">{result.reasonCode}</code>
              {result.degraded && (
                <Badge className="bg-red-100 text-red-700">{t("editor.degraded")}</Badge>
              )}
            </div>
            <div className="rounded-md bg-muted p-2 text-[11px]">
              <div>
                {t("editor.trialPackage", {
                  packageId: result.packageId,
                  packageVersion: result.packageVersion,
                  rulesetVersion: result.rulesetVersion,
                })}
              </div>
              <div className="mt-1">
                {t("editor.trialEvaluated", {
                  count: result.evaluatedRules,
                  latency: formatMs(result.executionTimeMs),
                  engine: result.engineVersion,
                })}
              </div>
            </div>
            {Object.keys(result.outputs ?? {}).length > 0 && (
              <div className="rounded-md bg-muted p-2 text-xs">
                <span className="text-muted-foreground">{t("editor.trialOutputs")}</span>
                {Object.entries(result.outputs).map(([k, v]) => (
                  <span key={k} className="me-2">
                    <code>{k}</code> = <code>{JSON.stringify(v)}</code>
                  </span>
                ))}
              </div>
            )}
            <div className="space-y-1">
              {result.hitRules.length === 0 && (
                <p className="text-xs text-muted-foreground">{t("editor.trialNoHit")}</p>
              )}
              {result.hitRules.map((h) => (
                <div
                  key={h.ruleId}
                  className="flex items-center justify-between gap-2 rounded bg-green-50 px-2 py-1 text-xs text-green-800"
                >
                  <span>
                    {h.ruleId}
                    <span className="ms-1 text-[10px] opacity-70">{h.ruleVersion}</span>
                  </span>
                  <span className="shrink-0">
                    {h.decision} · {h.reasonCode}
                  </span>
                </div>
              ))}
            </div>
            {result.degraded && result.degradedReasons.length > 0 && (
              <ul className="list-inside list-disc text-[11px] text-red-600">
                {result.degradedReasons.map((r, i) => (
                  <li key={i}>{r}</li>
                ))}
              </ul>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function formatMs(ms: number): string {
  return ms < 1 ? `${(ms * 1000).toFixed(0)} µs` : `${ms.toFixed(2)} ms`
}

function parseDef(
  text: string,
  topNotObject: string
): { ok: true; value: RulesetDefinition } | { ok: false; message: string } {
  try {
    const v = JSON.parse(text) as RulesetDefinition
    if (!v || typeof v !== "object") return { ok: false, message: topNotObject }
    return { ok: true, value: v }
  } catch (e) {
    return { ok: false, message: e instanceof Error ? e.message : String(e) }
  }
}

function toErr(e: unknown): ApiError {
  return e instanceof ApiError ? e : new ApiError(0, "UNKNOWN", String(e))
}

function renderCell(v: unknown): string {
  if (v === undefined) return "*"
  if (v === null) return "null"
  if (typeof v === "object") {
    const o = v as { op?: string; value?: unknown }
    if (o.op) return `${o.op} ${JSON.stringify(o.value)}`
    return JSON.stringify(v)
  }
  return String(v)
}

/** Expression -> readable text. Display only, never used for evaluation (evaluation always lives in the engine). */
function exprToText(e: unknown): string {
  if (e === null || typeof e !== "object") return JSON.stringify(e)
  const obj = e as {
    op?: string
    args?: unknown[]
    fact?: string
    type?: string
    value?: unknown
  }
  if (obj.fact !== undefined) return obj.fact
  if (obj.type !== undefined) return JSON.stringify(obj.value)
  if (obj.op) {
    const args = (obj.args ?? []).map(exprToText)
    switch (obj.op) {
      case "and":
        return args.join(" && ")
      case "or":
        return args.join(" || ")
      case "not":
        return `!(${args[0]})`
      case "eq":
        return `${args[0]} == ${args[1]}`
      case "gt":
        return `${args[0]} > ${args[1]}`
      case "gte":
        return `${args[0]} >= ${args[1]}`
      case "lt":
        return `${args[0]} < ${args[1]}`
      case "lte":
        return `${args[0]} <= ${args[1]}`
      case "in":
        return `${args[0]} in ${args[1]}`
      default:
        return `${obj.op}(${args.join(", ")})`
    }
  }
  return JSON.stringify(e)
}
