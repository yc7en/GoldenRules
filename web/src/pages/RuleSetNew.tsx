import { useEffect, useMemo, useState } from "react"
import { useTranslation } from "react-i18next"
import { Link, useNavigate } from "react-router-dom"
import { ArrowLeft, ListTree, Table2, SquareCode } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ErrorBox } from "@/components/async"
import { api, ApiError } from "@/lib/api"
import { useAsync } from "@/lib/useAsync"
import { blankDefinition } from "@/lib/ruleset/templates"
import { viewForKind, type AuthoringKind } from "@/lib/ruleset/kind"
import { cn } from "@/lib/utils"

/**
 * New ruleset.
 *
 * The authoring form is chosen here and nowhere else, because it decides the stored form: a table is
 * `table`, expressions and trees are both `rules[]`. The platform rejects a definition carrying
 * both, so "switch the form later" is not a UI gap - it would be a silent data loss.
 *
 * Each kind starts from a skeleton that already passes the backend dry-run. An empty rule set is not
 * an option: `Analyze` rejects zero rules, and a table with no rows, so an empty shell would fail the
 * moment it is saved and look like a platform bug.
 */
const KINDS: { kind: AuthoringKind; icon: typeof Table2 }[] = [
  { kind: "expression", icon: SquareCode },
  { kind: "table", icon: Table2 },
  { kind: "tree", icon: ListTree },
]

const ID_PATTERN = /^[a-z0-9._-]+$/

export default function RuleSetNew() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const facts = useAsync(() => api.facts.list(), [])
  const existing = useAsync(() => api.rulesets.list(), [])

  const [id, setId] = useState("")
  const [scene, setScene] = useState("")
  const [name, setName] = useState("")
  const [kind, setKind] = useState<AuthoringKind>("expression")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)

  const scenes = useMemo(() => {
    const set = new Set((existing.data?.items ?? []).map((r) => r.scene))
    return [...set].sort()
  }, [existing.data])

  const takenIds = useMemo(
    () => new Set((existing.data?.items ?? []).map((r) => r.rulesetId)),
    [existing.data]
  )

  // Offer the most common scenario once the list arrives, so the form is submittable in one step.
  useEffect(() => {
    if (!scene && scenes.length > 0) setScene(scenes[0])
  }, [scenes, scene])

  const trimmedId = id.trim()
  const idInvalid = trimmedId !== "" && !ID_PATTERN.test(trimmedId)
  const idTaken = trimmedId !== "" && takenIds.has(trimmedId)
  const canSubmit =
    trimmedId !== "" && !idInvalid && !idTaken && scene.trim() !== "" && !busy

  const submit = async () => {
    if (!canSubmit) return
    setBusy(true)
    setError(null)
    try {
      const def = blankDefinition(kind, {
        rulesetId: trimmedId,
        scene: scene.trim(),
        name: name.trim() || undefined,
        facts: facts.data?.items ?? [],
      })
      const report = await api.rulesets.save(trimmedId, def)
      if (!report.valid) {
        // Saving an invalid draft reports back instead of throwing, so the gate has to be explicit:
        // navigating away here would leave the author believing the rule set exists.
        const first = report.errors[0]
        setError(
          new ApiError(
            422,
            first?.code ?? "VALIDATION_FAILED",
            t("create.failed", { message: first?.message ?? "—" }),
            report
          )
        )
        return
      }
      navigate(`/rulesets/${encodeURIComponent(trimmedId)}?view=${viewForKind(kind)}`)
    } catch (e) {
      setError(e instanceof ApiError ? e : new ApiError(0, "UNKNOWN", String(e)))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4 p-6">
      <div className="flex items-center gap-3">
        <Link to="/rulesets">
          <Button variant="ghost" size="icon">
            <ArrowLeft className="size-4 rtl:rotate-180" />
          </Button>
        </Link>
        <div>
          <h1 className="text-lg font-semibold">{t("create.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("create.subtitle")}</p>
        </div>
      </div>

      {error && <ErrorBox error={error} />}

      <Card>
        <CardContent className="space-y-4 pt-4">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <label className="text-xs text-muted-foreground">
              {t("create.id")}
              <input
                className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1.5 font-mono text-sm"
                value={id}
                dir="ltr"
                autoFocus
                placeholder="rs-withdraw-rate"
                onChange={(e) => setId(e.target.value)}
              />
              <span
                className={cn(
                  "mt-1 block text-[11px]",
                  idInvalid || idTaken ? "text-red-600" : "text-muted-foreground"
                )}
              >
                {idInvalid ? t("create.invalidId") : idTaken ? t("create.taken") : t("create.idHint")}
              </span>
            </label>

            <label className="text-xs text-muted-foreground">
              {t("create.scene")}
              <input
                className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1.5 text-sm"
                value={scene}
                list="scene-options"
                placeholder={t("create.sceneHint")}
                onChange={(e) => setScene(e.target.value)}
              />
              <datalist id="scene-options">
                {scenes.map((s) => (
                  <option key={s} value={s} />
                ))}
              </datalist>
              <span className="mt-1 block text-[11px] text-muted-foreground">
                {t("create.sceneHint")}
              </span>
            </label>
          </div>

          <label className="block text-xs text-muted-foreground">
            {t("create.name")}
            <input
              className="mt-1 w-full rounded-md border border-input bg-transparent px-2 py-1.5 text-sm"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">{t("create.kind")}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-2">
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
            {KINDS.map(({ kind: k, icon: Icon }) => (
              <button
                key={k}
                type="button"
                onClick={() => setKind(k)}
                className={cn(
                  "rounded-lg border p-3 text-start transition-colors",
                  kind === k
                    ? "border-primary bg-muted/60 ring-1 ring-primary"
                    : "hover:bg-muted/40"
                )}
              >
                <div className="flex items-center gap-2 text-sm font-medium">
                  <Icon className="size-4" />
                  {t(`create.${k}`)}
                </div>
                <p className="mt-1 text-[11px] text-muted-foreground">
                  {t(`create.${k}Desc`)}
                </p>
              </button>
            ))}
          </div>
          <p className="text-[11px] text-muted-foreground">{t("create.kindHint")}</p>
        </CardContent>
      </Card>

      <div className="flex justify-end">
        <Button disabled={!canSubmit} onClick={submit}>
          {busy ? t("create.submitting") : t("create.submit")}
        </Button>
      </div>
    </div>
  )
}
