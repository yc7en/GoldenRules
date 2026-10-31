import { useTranslation } from "react-i18next"
import { Badge } from "@/components/ui/badge"
import { Async } from "@/components/async"
import { useAsync } from "@/lib/useAsync"
import { api } from "@/lib/api"
import type { RulesetDefinition } from "@/lib/api-types"

interface SceneGroup {
  scene: string
  rulesets: RulesetDefinition[]
  rules: number
  tests: number
  approvedTests: number
}

export default function Scenes() {
  const { t } = useTranslation()
  // A dead engine should not turn the whole page red: the platform-side rulesets are what this page is about.
  // Degrading the two backends separately is the only way to see at a glance whether "the engine is down" or "the platform is down".
  const state = useAsync(async () => {
    const list = await api.rulesets.list()
    const engine = await api.engine.info().catch(() => null)
    return { list, engine }
  }, [])

  return (
    <div className="p-6 space-y-4">
      <div>
        <h1 className="text-lg font-semibold">{t("scenes.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("scenes.subtitle")}</p>
      </div>

      <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
        {t("scenes.redline")}
        <span className="ms-1 opacity-80">{t("scenes.redlineNote")}</span>
      </div>

      <Async state={state} loadingLabel={t("scenes.loading")}>
        {({ list, engine }) => {
          const groups = groupByScene(list.items)
          const loaded = engine?.packages?.packages?.[0]
          return (
            <>
              {loaded ? (
                <div className="rounded-md border bg-card px-3 py-2 text-xs text-muted-foreground">
                  {t("scenes.engineLoaded")}{" "}
                  <code>{loaded.packageId}</code>{" "}
                  <code>{loaded.packageVersion}</code>
                  {t("scenes.engineLoadedStats", {
                    rulesets: t("scenes.rulesetsShort", { count: loaded.rulesets }),
                    rules: t("scenes.rulesShort", { count: loaded.rules }),
                    size: t("scenes.bytesShort", { bytes: loaded.estBytes }),
                  })}
                </div>
              ) : (
                <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
                  {t("scenes.engineDown")}
                </div>
              )}
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
                {groups.map((g) => (
                  <div
                    key={g.scene}
                    className="rounded-xl border bg-card p-4 shadow-sm"
                  >
                    <div className="mb-2 flex items-center justify-between gap-2">
                      <h2 className="font-semibold">{g.scene}</h2>
                      <Badge variant={g.rulesets.length > 0 ? "default" : "secondary"}>
                        {t("scenes.rulesetsShort", { count: g.rulesets.length })}
                      </Badge>
                    </div>
                    <p className="mb-3 text-xs text-muted-foreground">
                      {t("scenes.rulesShort", { count: g.rules })} ·{" "}
                      {t("scenes.stats", {
                        approved: g.approvedTests,
                        total: g.tests,
                      })}
                    </p>
                    <div className="space-y-2">
                      {g.rulesets.map((rs) => (
                        <div
                          key={rs.rulesetId}
                          className="flex items-center justify-between gap-2 rounded-md bg-muted px-3 py-2 text-sm"
                        >
                          <span className="min-w-0 truncate font-medium">
                            {rs.rulesetId}
                          </span>
                          <span className="shrink-0 text-xs text-muted-foreground">
                            {rs.hitStrategy} ·{" "}
                            {t("scenes.rulesetLine", {
                              rules: t("scenes.rulesShort", {
                                count: rs.rules?.length ?? 0,
                              }),
                              version: rs.version,
                            })}
                          </span>
                        </div>
                      ))}
                    </div>
                    {g.rulesets.length > 1 && (
                      <p className="mt-2 text-[11px] text-amber-700">
                        {t("scenes.conflict")}
                      </p>
                    )}
                  </div>
                ))}
              </div>
            </>
          )
        }}
      </Async>
    </div>
  )
}

function groupByScene(items: RulesetDefinition[]): SceneGroup[] {
  const map = new Map<string, SceneGroup>()
  for (const rs of items) {
    const g = map.get(rs.scene) ?? {
      scene: rs.scene,
      rulesets: [],
      rules: 0,
      tests: 0,
      approvedTests: 0,
    }
    g.rulesets.push(rs)
    g.rules += rs.rules?.length ?? 0
    const tests = rs.tests ?? []
    g.tests += tests.length
    g.approvedTests += tests.filter((t) => t.status === "approved").length
    map.set(rs.scene, g)
  }
  return [...map.values()].sort((a, b) => a.scene.localeCompare(b.scene))
}
