import type { ReactNode } from "react"
import { AlertTriangle, Loader2, RefreshCw, WifiOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { ApiError } from "@/lib/api"
import type { AsyncState } from "@/lib/useAsync"

/** Error code -> troubleshooting action. The backend's message says "what happened"; this adds "what to fix". */
const HINTS: Record<string, string> = {
  NETWORK_UNREACHABLE:
    "后端没起来，或平台的 --cors-origin 没开。跑 tools/dev-up.sh 拉起引擎与平台。",
  TIMEOUT: "请求超时。引擎在做长决策，或后端进程已卡死。",
  NOT_FOUND: "对象不存在。可能是数据目录被重置过，刷新列表重新选。",
  PKG_VERIFY_UNAVAILABLE:
    "平台没有配置门禁执行器（--engine-url）。带 approved 用例的包会因此发不出去。",
  VERSION_SELECTION_CONFLICT:
    "该场景挂了多个规则集，请求必须指定 rulesetId。",
}

export function ErrorBox({
  error,
  onRetry,
}: {
  error: ApiError
  onRetry?: () => void
}) {
  const hint = HINTS[error.code]
  return (
    <div className="rounded-lg border border-red-200 bg-red-50 p-4">
      <div className="flex items-start gap-2">
        {error.isUnreachable ? (
          <WifiOff className="mt-0.5 size-4 shrink-0 text-red-600" />
        ) : (
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-red-600" />
        )}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <code className="rounded bg-red-100 px-1.5 py-0.5 text-xs font-medium text-red-800">
              {error.code}
            </code>
            {error.status > 0 && (
              <span className="text-xs text-red-500">HTTP {error.status}</span>
            )}
          </div>
          <p className="mt-1 break-words text-sm text-red-800">{error.message}</p>
          {hint && <p className="mt-1 text-xs text-red-600">怎么办：{hint}</p>}
        </div>
        {onRetry && (
          <Button variant="outline" size="sm" onClick={onRetry}>
            <RefreshCw className="size-3.5" />
            重试
          </Button>
        )}
      </div>
    </div>
  )
}

export function Loading({ label = "加载中" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border bg-card p-4 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      {label}…
    </div>
  )
}

/**
 * The single exit point for the three states of a fetch.
 *
 * The three states must be rendered separately: drawing "loading" and "empty data" as the same blank
 * makes people think the backend returned an empty list - these two conclusions point troubleshooting in
 * completely opposite directions.
 */
export function Async<T>({
  state,
  children,
  empty,
  loadingLabel,
}: {
  state: AsyncState<T> & { reload: () => void }
  children: (data: T) => ReactNode
  empty?: (data: T) => boolean
  loadingLabel?: string
}) {
  if (state.loading && state.data === null) return <Loading label={loadingLabel} />
  if (state.error) return <ErrorBox error={state.error} onRetry={state.reload} />
  if (state.data === null) return <Loading label={loadingLabel} />
  if (empty && empty(state.data)) {
    return (
      <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
        接口返回了空集合。这不是加载失败 —— 后端确实没有数据。
      </div>
    )
  }
  return <>{children(state.data)}</>
}
