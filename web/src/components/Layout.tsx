import { useEffect, useState } from "react"
import { NavLink, Outlet } from "react-router-dom"
import { cn } from "@/lib/utils"
import { api, getActor, setActor } from "@/lib/api"
import type { Actor } from "@/lib/api"
import type { ActorType } from "@/lib/api-types"
import {
  LayoutGrid,
  ListTree,
  Database,
  TestTube2,
  GitPullRequest,
  Activity,
  ScrollText,
} from "lucide-react"

const navItems = [
  { to: "/scenes", label: "场景策略", icon: LayoutGrid },
  { to: "/rulesets", label: "规则集", icon: ListTree },
  { to: "/facts", label: "事实目录", icon: Database },
  { to: "/testcases", label: "测试用例", icon: TestTube2 },
  { to: "/approval", label: "审批与发布", icon: GitPullRequest },
  { to: "/observability", label: "观测与审计", icon: Activity },
]

const ACTOR_TYPES: ActorType[] = [
  "engineer",
  "peer",
  "business",
  "risk",
  "compliance",
  "owner",
]

type Health = "up" | "down" | "checking"

export default function Layout() {
  const [engine, setEngine] = useState<Health>("checking")
  const [platform, setPlatform] = useState<Health>("checking")
  const [actor, setLocalActor] = useState<Actor>(getActor)

  // Probe health every 15 seconds. The sidebar used to hard-code "decision service - healthy",
  // which stayed green even with the backend down - a green light like that is worse than no indicator.
  useEffect(() => {
    let alive = true
    const probe = async () => {
      const [p, e] = await Promise.allSettled([
        api.platformHealth(),
        api.engine.info(),
      ])
      if (!alive) return
      setPlatform(p.status === "fulfilled" ? "up" : "down")
      setEngine(e.status === "fulfilled" ? "up" : "down")
    }
    probe()
    const timer = setInterval(probe, 15_000)
    return () => {
      alive = false
      clearInterval(timer)
    }
  }, [])

  const applyActor = (next: Actor) => {
    setActor(next)
    setLocalActor(next)
    // The approval page depends on this signal to refetch after an actor switch: the actor on this page changed, and the status of tickets approved by others changed with it
    window.dispatchEvent(new Event("gr:actor-changed"))
  }

  return (
    <div className="flex h-screen w-full overflow-hidden">
      {/* Sidebar */}
      <aside className="flex w-56 shrink-0 flex-col border-r bg-card">
        <div className="flex h-14 items-center gap-2 border-b px-4">
          <ScrollText className="size-5 text-primary" />
          <span className="text-sm font-semibold">GoldenRules BRMS</span>
        </div>
        <nav className="flex-1 space-y-1 overflow-auto p-2">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                cn(
                  "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                  isActive
                    ? "bg-primary text-primary-foreground"
                    : "text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                )
              }
            >
              <item.icon className="size-4" />
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="space-y-2 border-t p-3 text-xs text-muted-foreground">
          <HealthRow label="决策服务" state={engine} hint="Rust 引擎，试跑与决策" />
          <HealthRow label="管理平台" state={platform} hint="Go 平台，规则与审批" />

          <div className="pt-1">
            <label className="mb-1 block text-[11px] uppercase tracking-wide">
              当前操作者
            </label>
            <div className="flex gap-1">
              <input
                value={actor.id}
                onChange={(e) => applyActor({ ...actor, id: e.target.value })}
                className="min-w-0 flex-1 rounded border border-input bg-transparent px-1.5 py-1 font-mono text-[11px]"
                aria-label="操作者 ID"
              />
              <select
                value={actor.type}
                onChange={(e) =>
                  applyActor({ ...actor, type: e.target.value as ActorType })
                }
                className="rounded border border-input bg-transparent px-1 py-1 text-[11px]"
                aria-label="操作者类型"
              >
                {ACTOR_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </div>
            <p className="mt-1 text-[10px] leading-tight text-muted-foreground/80">
              审批记名与「自审」判定都取这里；换人才能走完职责分离。
            </p>
          </div>
        </div>
      </aside>

      {/* Main content area */}
      <main className="flex-1 overflow-auto bg-muted/30">
        <Outlet />
      </main>
    </div>
  )
}

function HealthRow({
  label,
  state,
  hint,
}: {
  label: string
  state: Health
  hint: string
}) {
  const dot =
    state === "up"
      ? "bg-green-500"
      : state === "down"
      ? "bg-red-500"
      : "bg-muted-foreground/40"
  const text =
    state === "up" ? "正常" : state === "down" ? "连不上" : "探测中"
  return (
    <div className="flex items-center justify-between" title={hint}>
      <div className="flex items-center gap-2">
        <span className={cn("inline-block size-2 rounded-full", dot)} />
        {label}
      </div>
      <span className={state === "down" ? "text-red-600" : ""}>{text}</span>
    </div>
  )
}
