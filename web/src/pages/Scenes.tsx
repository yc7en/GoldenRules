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
        <h1 className="text-lg font-semibold">场景策略</h1>
        <p className="text-sm text-muted-foreground">
          场景挂载规则集（列表配置，非节点连线画布）
        </p>
      </div>

      <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
        红线：这里不是「流画布」。场景策略 = 挂载哪些规则集的列表，不做节点拖拽连线编排。
        <span className="ml-1 opacity-80">
          本页的「场景」由规则集上声明的 scene 分组而来 —— 后端暂无独立的场景实体。
        </span>
      </div>

      <Async state={state} loadingLabel="读取场景与规则集">
        {({ list, engine }) => {
          const groups = groupByScene(list.items)
          const loaded = engine?.packages?.packages?.[0]
          return (
            <>
              {loaded ? (
                <div className="rounded-md border bg-card px-3 py-2 text-xs text-muted-foreground">
                  引擎当前装载：<code>{loaded.packageId}</code>{" "}
                  <code>{loaded.packageVersion}</code> · {loaded.rulesets} 个规则集 ·{" "}
                  {loaded.rules} 条规则 · 预估 {loaded.estBytes} 字节
                </div>
              ) : (
                <div className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800">
                  引擎未连接：下面是平台里的配置，不代表引擎正在执行什么。试跑与决策会失败。
                </div>
              )}
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
                {groups.map((g) => (
                  <div
                    key={g.scene}
                    className="rounded-xl border bg-card p-4 shadow-sm"
                  >
                    <div className="mb-2 flex items-center justify-between">
                      <h2 className="font-semibold">{g.scene}</h2>
                      <Badge variant={g.rulesets.length > 0 ? "default" : "secondary"}>
                        {g.rulesets.length} 个规则集
                      </Badge>
                    </div>
                    <p className="mb-3 text-xs text-muted-foreground">
                      共 {g.rules} 条规则 · 用例 {g.approvedTests}/{g.tests} 条已确认
                    </p>
                    <div className="space-y-2">
                      {g.rulesets.map((rs) => (
                        <div
                          key={rs.rulesetId}
                          className="flex items-center justify-between rounded-md bg-muted px-3 py-2 text-sm"
                        >
                          <span className="font-medium">{rs.rulesetId}</span>
                          <span className="text-xs text-muted-foreground">
                            {rs.hitStrategy} · {rs.rules?.length ?? 0} 规则 ·{" "}
                            {rs.version}
                          </span>
                        </div>
                      ))}
                    </div>
                    {g.rulesets.length > 1 && (
                      <p className="mt-2 text-[11px] text-amber-700">
                        该场景挂载多个规则集：调用方必须指定 rulesetId，否则引擎返回
                        VERSION_SELECTION_CONFLICT。
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
