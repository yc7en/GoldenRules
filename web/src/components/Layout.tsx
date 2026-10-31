import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"
import { NavLink, Outlet } from "react-router-dom"
import { cn } from "@/lib/utils"
import { api, getActor, setActor } from "@/lib/api"
import type { Actor } from "@/lib/api"
import type { ActorType } from "@/lib/api-types"
import { LanguageSwitcher } from "@/components/LanguageSwitcher"
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
  { to: "/scenes", labelKey: "nav.scenes", icon: LayoutGrid },
  { to: "/rulesets", labelKey: "nav.rulesets", icon: ListTree },
  { to: "/facts", labelKey: "nav.facts", icon: Database },
  { to: "/testcases", labelKey: "nav.testcases", icon: TestTube2 },
  { to: "/approval", labelKey: "nav.approval", icon: GitPullRequest },
  { to: "/observability", labelKey: "nav.observability", icon: Activity },
] as const

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
  const { t } = useTranslation()
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
      <aside className="flex w-56 shrink-0 flex-col border-e bg-card">
        <div className="flex h-14 items-center gap-2 border-b px-4">
          <ScrollText className="size-5 text-primary" />
          <span className="text-sm font-semibold">{t("app.title")}</span>
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
              {t(item.labelKey)}
            </NavLink>
          ))}
        </nav>

        <div className="space-y-2 border-t p-3 text-xs text-muted-foreground">
          <HealthRow
            label={t("health.engine")}
            state={engine}
            hint={t("health.engineHint")}
          />
          <HealthRow
            label={t("health.platform")}
            state={platform}
            hint={t("health.platformHint")}
          />

          <div className="pt-1">
            <label className="mb-1 block text-[11px] uppercase tracking-wide">
              {t("actor.label")}
            </label>
            <div className="flex gap-1">
              <input
                value={actor.id}
                onChange={(e) => applyActor({ ...actor, id: e.target.value })}
                className="min-w-0 flex-1 rounded border border-input bg-transparent px-1.5 py-1 font-mono text-[11px]"
                aria-label={t("actor.idAria")}
              />
              <select
                value={actor.type}
                onChange={(e) =>
                  applyActor({ ...actor, type: e.target.value as ActorType })
                }
                className="rounded border border-input bg-transparent px-1 py-1 text-[11px]"
                aria-label={t("actor.typeAria")}
              >
                {/* Contract enum values stay untranslated; only the label around them changes. */}
                {ACTOR_TYPES.map((v) => (
                  <option key={v} value={v}>
                    {v}
                  </option>
                ))}
              </select>
            </div>
            <p className="mt-1 text-[10px] leading-tight text-muted-foreground/80">
              {t("actor.hint")}
            </p>
          </div>

          <div className="border-t pt-2">
            <LanguageSwitcher />
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
  const { t } = useTranslation()
  const dot =
    state === "up"
      ? "bg-green-500"
      : state === "down"
      ? "bg-red-500"
      : "bg-muted-foreground/40"
  const text =
    state === "up"
      ? t("health.up")
      : state === "down"
      ? t("health.down")
      : t("health.checking")
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
