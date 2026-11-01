import { useEffect, useMemo, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { AlertTriangle, Info, Trash2 } from "lucide-react"
import type { FactRecord, Rule, RuleThen, RulesetDefinition, TrialResult } from "@/lib/api-types"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { compileTree } from "@/lib/tree/compile"
import { analyzeRuleset } from "@/lib/tree/decompile"
import {
  defaultCond,
  findNode,
  literalText,
  newCondNode,
  newLeafNode,
  opsForFact,
  parseLiteralInput,
  removeNode,
  setSlot,
  TREE_OPS,
  updateCond,
  updateLeaf,
} from "@/lib/tree/edit"
import { condToText, type Cond, type Slot } from "@/lib/tree/model"
import { TreeCanvas } from "./TreeCanvas"

export interface TreeViewProps {
  ruleset: RulesetDefinition
  facts: FactRecord[]
  rtl: boolean
  trial: TrialResult | null
  /** Compiled rules go back to the editor, which owns the definition and the save button. */
  onRulesChange: (rules: Rule[]) => void
}

export function TreeView({ ruleset, facts, rtl, trial, onRulesChange }: TreeViewProps) {
  const { t } = useTranslation()
  const analysis = useMemo(() => analyzeRuleset(ruleset), [ruleset])
  const [root, setRoot] = useState<Slot>(analysis.root ?? null)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [layoutKey, setLayoutKey] = useState(0)

  /**
   * The last rule set this canvas pushed upward. Kept so an incoming change can be classified: if it
   * is the echo of our own edit, the local tree is already what produced it and resetting would only
   * drop the selection. Anything else came from the JSON tab or a reload, and following it is the
   * only way the two views stay the same document.
   */
  const emitted = useRef<string | null>(null)
  const incoming = JSON.stringify(ruleset.rules ?? [])
  useEffect(() => {
    if (emitted.current === incoming) return
    emitted.current = incoming
    setRoot(analysis.root ?? null)
    setSelectedId(null)
  }, [incoming, analysis])

  const editable = analysis.level === "L1"

  const hitRuleIds = useMemo(() => {
    if (!trial) return []
    // Only a hit path is lit; no trial means nothing is greyed out either, so "no result yet" and
    // "nothing matched" never look the same.
    return trial.hitRules.map((h) => h.ruleId)
  }, [trial])

  const commit = (next: Slot) => {
    setRoot(next)
    const rules = compileTree(next, {
      scene: ruleset.scene,
      usedIds: (ruleset.rules ?? []).map((r) => r.ruleId),
    })
    emitted.current = JSON.stringify(rules)
    onRulesChange(rules)
  }

  const handleAdd = (parentId: string | null, branch: "yes" | "no", kind: "cond" | "leaf") => {
    const node =
      kind === "cond"
        ? newCondNode(defaultCond(facts))
        : newLeafNode({ decision: "APPROVE", reasonCode: "OK" })
    commit(setSlot(root, parentId, branch, node))
    setSelectedId(node.id)
  }

  const selected = selectedId ? findNode(root, selectedId) : null

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant={editable ? "secondary" : "outline"}>
            {t(`tree.level.${analysis.level}`)}
          </Badge>
          <span className="text-xs text-muted-foreground">
            {t("tree.summary", {
              rules: analysis.ruleCount,
              paths: analysis.flatRules?.length ?? analysis.ruleCount,
            })}
          </span>
        </div>
        <Button variant="outline" size="sm" onClick={() => setLayoutKey((k) => k + 1)}>
          {t("tree.relayout")}
        </Button>
      </div>

      {analysis.reasonKey && (
        <div className="flex items-start gap-2 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
          <span>{t(analysis.reasonKey, analysis.reasonVars)}</span>
        </div>
      )}

      {editable && (
        <div className="flex items-start gap-2 rounded-md border border-blue-200 bg-blue-50 px-3 py-2 text-[11px] text-blue-800">
          <Info className="mt-0.5 size-3 shrink-0" />
          <span>{t("tree.editHint")}</span>
        </div>
      )}

      <TreeCanvas
        analysis={editable ? { ...analysis, root } : analysis}
        facts={facts}
        rtl={rtl}
        editable={editable}
        hitRuleIds={hitRuleIds}
        labels={{
          yes: t("tree.edge.yes"),
          no: t("tree.edge.no"),
          entry: t("tree.node.entry"),
          aggregate: t("tree.node.aggregate", { strategy: ruleset.hitStrategy ?? "ALL" }),
          addCond: t("tree.add.cond"),
          addLeaf: t("tree.add.leaf"),
          placeholderCond: t("tree.placeholder.cond"),
          placeholderLeaf: t("tree.placeholder.leaf"),
        }}
        selectedId={selectedId}
        onSelect={setSelectedId}
        onAdd={handleAdd}
        layoutKey={layoutKey}
      />

      {editable && (
        <Card>
          <CardHeader className="py-2">
            <CardTitle className="text-sm">{t("tree.inspector.title")}</CardTitle>
          </CardHeader>
          <CardContent className="pt-0">
            {!selected ? (
              <p className="text-xs text-muted-foreground">{t("tree.inspector.none")}</p>
            ) : selected.kind === "cond" ? (
              <CondInspector
                nodeId={selected.id}
                cond={selected.cond}
                facts={facts}
                onChange={(cond) => commit(updateCond(root, selected.id, cond))}
                onDelete={() => {
                  commit(removeNode(root, selected.id))
                  setSelectedId(null)
                }}
              />
            ) : (
              <LeafInspector
                then={selected.then}
                ruleId={selected.originRuleId}
                onChange={(then) => commit(updateLeaf(root, selected.id, then))}
                onDelete={() => {
                  commit(removeNode(root, selected.id))
                  setSelectedId(null)
                }}
              />
            )}
          </CardContent>
        </Card>
      )}
    </div>
  )
}

function CondInspector({
  nodeId,
  cond,
  facts,
  onChange,
  onDelete,
}: {
  nodeId: string
  cond: Cond
  facts: FactRecord[]
  onChange: (cond: Cond) => void
  onDelete: () => void
}) {
  const { t } = useTranslation()
  const fact = facts.find((f) => f.fact === cond.fact)
  const ops = opsForFact(fact).filter((op) => TREE_OPS.includes(op as never))
  const [draft, setDraft] = useState(() => literalText(cond.value))

  useEffect(() => {
    setDraft(literalText(cond.value))
  }, [nodeId, cond.value])

  return (
    <div className="space-y-2">
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
        <label className="text-[11px] text-muted-foreground">
          {t("tree.inspector.fact")}
          <select
            className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1 text-xs"
            value={cond.fact}
            onChange={(e) => {
              const nextFact = facts.find((f) => f.fact === e.target.value)
              const value = nextFact?.enum?.[0]
              onChange({
                fact: e.target.value,
                op: opsForFact(nextFact).includes(cond.op) ? cond.op : "eq",
                value: nextFact
                  ? { type: nextFact.type === "enum" ? "string" : nextFact.type, value: value ?? "" }
                  : cond.value,
              })
            }}
          >
            {facts.map((f) => (
              <option key={f.fact} value={f.fact}>
                {f.displayName ? `${f.displayName} (${f.fact})` : f.fact}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[11px] text-muted-foreground">
          {t("tree.inspector.op")}
          <select
            className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1 text-xs"
            value={cond.op}
            onChange={(e) => onChange({ ...cond, op: e.target.value })}
          >
            {ops.map((op) => (
              <option key={op} value={op}>
                {op}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[11px] text-muted-foreground">
          {t("tree.inspector.value")}
          {fact?.type === "enum" && cond.op !== "in" && cond.op !== "notIn" ? (
            <select
              className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1 text-xs"
              value={String(cond.value.value)}
              onChange={(e) =>
                onChange({ ...cond, value: { type: "string", value: e.target.value } })
              }
            >
              {(fact.enum ?? []).map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </select>
          ) : (
            <input
              className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1 font-mono text-xs"
              value={draft}
              dir="ltr"
              onChange={(e) => setDraft(e.target.value)}
              onBlur={() => onChange({ ...cond, value: parseLiteralInput(fact, cond, draft) })}
            />
          )}
        </label>
      </div>
      <div className="flex items-center justify-between gap-2">
        <code className="text-[11px] text-muted-foreground" dir="ltr">
          {condToText(cond)}
        </code>
        <Button variant="ghost" size="sm" className="text-red-600" onClick={onDelete}>
          <Trash2 className="size-3.5" />
          {t("tree.inspector.delete")}
        </Button>
      </div>
    </div>
  )
}

function LeafInspector({
  then,
  ruleId,
  onChange,
  onDelete,
}: {
  then: RuleThen
  ruleId?: string
  onChange: (then: RuleThen) => void
  onDelete: () => void
}) {
  const { t } = useTranslation()
  const [reason, setReason] = useState(then.reasonCode)
  useEffect(() => setReason(then.reasonCode), [ruleId, then.reasonCode])

  return (
    <div className="space-y-2">
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
        <label className="text-[11px] text-muted-foreground">
          {t("tree.inspector.decision")}
          <select
            className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1 text-xs"
            value={then.decision}
            onChange={(e) => onChange({ ...then, decision: e.target.value })}
          >
            {["APPROVE", "REJECT", "REVIEW", "ROUTE"].map((d) => (
              <option key={d} value={d}>
                {d}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[11px] text-muted-foreground sm:col-span-2">
          {t("tree.inspector.reasonCode")}
          <input
            className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1 font-mono text-xs"
            value={reason}
            dir="ltr"
            onChange={(e) => setReason(e.target.value)}
            onBlur={() => onChange({ ...then, reasonCode: reason })}
          />
        </label>
      </div>
      <div className="flex items-center justify-between gap-2">
        <span className="font-mono text-[11px] text-muted-foreground" dir="ltr">
          {ruleId ?? t("tree.inspector.newRule")}
        </span>
        <Button variant="ghost" size="sm" className="text-red-600" onClick={onDelete}>
          <Trash2 className="size-3.5" />
          {t("tree.inspector.delete")}
        </Button>
      </div>
    </div>
  )
}
