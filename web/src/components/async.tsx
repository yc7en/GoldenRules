import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"
import type { TFunction } from "i18next"
import { AlertTriangle, Loader2, RefreshCw, WifiOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { ApiError } from "@/lib/api"
import type { AsyncState } from "@/lib/useAsync"

/**
 * Error code -> localized "what happened and what to fix".
 *
 * `errorCode` is the contract and `message` is an implementation detail: the backend's message is
 * Chinese diagnostic text, so using it as UI copy would leak the backend language into every
 * locale. The raw message stays available, collapsed, and is never discarded - an unrecognized
 * code must not be flattened into a generic "something went wrong".
 */
function hintFor(t: TFunction, code: string): string {
  switch (code) {
    case "NETWORK_UNREACHABLE":
      return t("errors.hint.NETWORK_UNREACHABLE")
    case "TIMEOUT":
      return t("errors.hint.TIMEOUT")
    case "NOT_FOUND":
      return t("errors.hint.NOT_FOUND")
    case "PKG_VERIFY_UNAVAILABLE":
      return t("errors.hint.PKG_VERIFY_UNAVAILABLE")
    case "VERSION_SELECTION_CONFLICT":
      return t("errors.hint.VERSION_SELECTION_CONFLICT")
    // Both version errors come back from the engine during a trial run, and both say the same thing in
    // different words: what you are trialing is not what the engine has loaded. Without a hint the box
    // renders "unrecognized error code" next to a perfectly clear backend message (a rule set created in
    // the platform is never in the engine's package until it is published and hot-reloaded).
    case "VERSION_RULESET_NOT_FOUND":
      return t("errors.hint.VERSION_RULESET_NOT_FOUND")
    case "VERSION_PACKAGE_NOT_FOUND":
      return t("errors.hint.VERSION_PACKAGE_NOT_FOUND")
    default:
      return ""
  }
}

export function ErrorBox({
  error,
  onRetry,
}: {
  error: ApiError
  onRetry?: () => void
}) {
  const { t } = useTranslation()
  const hint = hintFor(t, error.code)
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
            {!hint && (
              <span className="text-[11px] text-red-500">{t("errors.unknownCode")}</span>
            )}
          </div>
          <p className="mt-1 break-words text-sm text-red-800">
            {hint || error.message}
          </p>
          {hint && (
            <details className="mt-1">
              <summary className="cursor-pointer text-[11px] text-red-600">
                {t("errors.backendRaw")}
              </summary>
              <p className="mt-1 break-words font-mono text-[11px] text-red-700">
                {error.message}
              </p>
            </details>
          )}
        </div>
        {onRetry && (
          <Button variant="outline" size="sm" onClick={onRetry}>
            <RefreshCw className="size-3.5" />
            {t("common.retry")}
          </Button>
        )}
      </div>
    </div>
  )
}

export function Loading({ label }: { label?: string }) {
  const { t } = useTranslation()
  return (
    <div className="flex items-center gap-2 rounded-lg border bg-card p-4 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      {label ?? t("common.loading")}…
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
  const { t } = useTranslation()
  if (state.loading && state.data === null) return <Loading label={loadingLabel} />
  if (state.error) return <ErrorBox error={state.error} onRetry={state.reload} />
  if (state.data === null) return <Loading label={loadingLabel} />
  if (empty && empty(state.data)) {
    return (
      <div className="rounded-lg border bg-card p-6 text-sm text-muted-foreground">
        {t("common.emptySet")}
      </div>
    )
  }
  return <>{children(state.data)}</>
}
