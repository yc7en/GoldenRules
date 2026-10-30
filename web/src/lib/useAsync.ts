import { useCallback, useEffect, useRef, useState } from "react"
import { ApiError } from "@/lib/api"

export interface AsyncState<T> {
  data: T | null
  error: ApiError | null
  loading: boolean
  /** Time of the last successful fetch, used to tell the user on screen "how stale this data is" */
  loadedAt: number | null
}

/**
 * Fetch hook: one per page.
 *
 * Why not React Query: there is only "fetch once + manual retry" here, so pulling in a state machine
 * library would only force us to explain its cache invalidation semantics. The loader is kept in a ref
 * so callers do not trigger infinite refetching by passing a new function on every render - a trap you
 * almost inevitably hit when there is no dependency array constraint.
 */
export function useAsync<T>(
  loader: () => Promise<T>,
  deps: unknown[] = []
): AsyncState<T> & { reload: () => void } {
  const [state, setState] = useState<AsyncState<T>>({
    data: null,
    error: null,
    loading: true,
    loadedAt: null,
  })
  const loaderRef = useRef(loader)
  loaderRef.current = loader
  const [tick, setTick] = useState(0)
  // Stop calling setState after unmount: an abandoned request coming back while switching pages would warn
  const aliveRef = useRef(true)

  useEffect(() => {
    aliveRef.current = true
    return () => {
      aliveRef.current = false
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    setState((s) => ({ ...s, loading: true, error: null }))
    loaderRef
      .current()
      .then((data) => {
        if (cancelled || !aliveRef.current) return
        setState({ data, error: null, loading: false, loadedAt: Date.now() })
      })
      .catch((err: unknown) => {
        if (cancelled || !aliveRef.current) return
        const error =
          err instanceof ApiError
            ? err
            : new ApiError(0, "UNKNOWN", String(err))
        setState((s) => ({ ...s, error, loading: false }))
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick])

  // Switching actor means switching "who is looking": approval slots, self-approval checks and audit attribution all change, so a refetch is required.
  useEffect(() => {
    const onChange = () => setTick((t) => t + 1)
    window.addEventListener("gr:actor-changed", onChange)
    return () => window.removeEventListener("gr:actor-changed", onChange)
  }, [])

  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { ...state, reload }
}
