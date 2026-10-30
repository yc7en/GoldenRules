import { useCallback, useEffect, useRef, useState } from "react"
import { ApiError } from "@/lib/api"

export interface AsyncState<T> {
  data: T | null
  error: ApiError | null
  loading: boolean
  /** 上次成功取数的时间，用于在界面上说明「这屏数据有多旧」 */
  loadedAt: number | null
}

/**
 * 取数 hook：一个页面一个。
 *
 * 为什么不用 React Query：这里只有「拉一次 + 手动重试」，引一个状态机库
 * 反而要解释它的缓存失效语义。用 ref 存 loader 是为了避免调用方每次渲染
 * 都传一个新函数导致无限重取——这个坑在没有依赖数组约束时几乎必然踩到。
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
  // 卸载后不再 setState：切页面时抛弃中的请求回来会报 warning
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

  // 换操作者等于换了「谁在看」：审批位、自审判定、审计归属都会变，必须重取。
  useEffect(() => {
    const onChange = () => setTick((t) => t + 1)
    window.addEventListener("gr:actor-changed", onChange)
    return () => window.removeEventListener("gr:actor-changed", onChange)
  }, [])

  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { ...state, reload }
}
