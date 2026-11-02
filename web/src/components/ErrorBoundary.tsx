import { Component, type ErrorInfo, type ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"

/**
 * Last-resort boundary around the whole console.
 *
 * Without it a single bad payload in a page component unmounts the entire React tree: the user gets a
 * blank white page with no error, no console hint on screen and no way to tell "still loading" from
 * "already dead". That is worse than the error itself, because the natural reaction is to retype the
 * data that is still sitting safely on the server.
 *
 * It deliberately does **not** try to recover automatically: a render error is a bug, and silently
 * re-rendering the same state usually just reproduces it. Reload is the honest action.
 */
interface BoundaryProps {
  children: ReactNode
  title: string
  hint: string
  reload: string
  detailLabel: string
}

interface BoundaryState {
  message: string
  stack: string
}

class Boundary extends Component<BoundaryProps, BoundaryState> {
  state: BoundaryState = { message: "", stack: "" }

  static getDerivedStateFromError(error: unknown): Partial<BoundaryState> {
    return { message: error instanceof Error ? error.message : String(error) }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // The component stack is the part that actually locates the bug; keep it next to the message.
    this.setState({ stack: info.componentStack ?? "" })
    console.error("[ui] render crash", error, info.componentStack)
  }

  render() {
    const { title, hint, reload, detailLabel, children } = this.props
    if (!this.state.message) return children
    return (
      <div className="flex min-h-dvh items-start justify-center bg-background p-6">
        <div className="w-full max-w-2xl space-y-3 rounded-lg border border-red-200 bg-red-50 p-4 text-red-900">
          <h1 className="text-base font-semibold">{title}</h1>
          <p className="text-sm">{hint}</p>
          <div className="space-y-1">
            <div className="text-[11px] font-medium uppercase tracking-wide opacity-70">{detailLabel}</div>
            <pre
              dir="ltr"
              className="max-h-64 overflow-auto rounded bg-white/70 p-2 text-left text-[11px] leading-relaxed"
            >
              {this.state.message}
              {this.state.stack}
            </pre>
          </div>
          <Button variant="outline" size="sm" onClick={() => window.location.reload()}>
            {reload}
          </Button>
        </div>
      </div>
    )
  }
}

/** Keeps the class component translation-ready: hooks cannot be used inside a class. */
export default function ErrorBoundary({ children }: { children: ReactNode }) {
  const { t } = useTranslation()
  return (
    <Boundary
      title={t("errors.uiCrashTitle")}
      hint={t("errors.uiCrashHint")}
      reload={t("errors.uiCrashReload")}
      detailLabel={t("errors.uiCrashDetail")}
    >
      {children}
    </Boundary>
  )
}
