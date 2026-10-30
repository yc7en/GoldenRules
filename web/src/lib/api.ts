import type {
  ActorType,
  ApprovalMatrixResponse,
  AuditListResponse,
  ChangeDetail,
  ChangeListResponse,
  EngineInfo,
  FactCatalogResponse,
  GatePreviewResponse,
  LogListResponse,
  LogStats,
  PackageListResponse,
  ReplayResponse,
  RulesetDefinition,
  RulesetListResponse,
  TrialResult,
  ValidationReport,
} from "@/lib/api-types"

/**
 * The UI only talks to the platform (Go). The engine (Rust) is reached through the platform -
 * the engine has no CORS enabled; it is an unauthenticated internal service and the browser should
 * never be able to call it directly.
 */
export const PLATFORM_BASE = trimBase(
  import.meta.env.VITE_PLATFORM_BASE ?? "http://127.0.0.1:18081"
)

function trimBase(s: string): string {
  return s.replace(/\/+$/, "")
}

export class ApiError extends Error {
  status: number
  code: string
  detail: unknown

  constructor(status: number, code: string, message: string, detail?: unknown) {
    super(message)
    this.name = "ApiError"
    this.status = status
    this.code = code
    this.detail = detail
  }

  /** The request never reached the backend at all (service down, CORS blocked, DNS failure) - the troubleshooting direction is completely different */
  get isUnreachable(): boolean {
    return this.status === 0
  }
}

// ---------------------------------------------------------------- Actor

export interface Actor {
  id: string
  type: ActorType
}

const ACTOR_KEY = "gr.actor"
const DEFAULT_ACTOR: Actor = { id: "engineer.web", type: "engineer" }

/**
 * Who the actor is determines whose name goes on the approval record, and whether "self-approval" is
 * rejected. Kept in localStorage rather than hard-coded: the approval matrix by design requires a
 * different person to act, and a hard-coded actor makes separation of duties untestable.
 */
export function getActor(): Actor {
  try {
    const raw = localStorage.getItem(ACTOR_KEY)
    if (raw) {
      const parsed = JSON.parse(raw) as Partial<Actor>
      if (parsed.id && parsed.type) return { id: parsed.id, type: parsed.type }
    }
  } catch {
    // When localStorage is disabled, fall back to the default actor instead of taking the whole page down
  }
  return DEFAULT_ACTOR
}

export function setActor(actor: Actor): void {
  localStorage.setItem(ACTOR_KEY, JSON.stringify(actor))
}

// ---------------------------------------------------------------- Request skeleton

interface RequestOptions {
  /** These status codes do not throw; the caller reads the body instead (a 422 validation diagnosis is a "failure with content") */
  accept?: number[]
  timeoutMs?: number
  signal?: AbortSignal
}

const DEFAULT_TIMEOUT = 10_000

async function request<T>(
  base: string,
  path: string,
  init: RequestInit = {},
  opts: RequestOptions = {}
): Promise<T> {
  const actor = getActor()
  const headers = new Headers(init.headers)
  if (init.body !== undefined && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json")
  }
  headers.set("X-Actor", actor.id)
  headers.set("X-Actor-Type", actor.type)

  // AbortSignal.timeout and the caller's own signal must be merged: keeping only one would disable
  // either "cancel when the user leaves the page" or "timeout protection".
  const timeout = AbortSignal.timeout(opts.timeoutMs ?? DEFAULT_TIMEOUT)
  const signal = opts.signal
    ? AbortSignal.any([opts.signal, timeout])
    : timeout

  let res: Response
  try {
    res = await fetch(base + path, { ...init, headers, signal })
  } catch (err) {
    const isAbort = err instanceof DOMException && err.name === "TimeoutError"
    throw new ApiError(
      0,
      isAbort ? "TIMEOUT" : "NETWORK_UNREACHABLE",
      isAbort
        ? `${base} 在 ${(opts.timeoutMs ?? DEFAULT_TIMEOUT) / 1000}s 内没有响应`
        : `连不上 ${base}。请确认后端已启动（tools/dev-up.sh），且平台带 --cors-origin 启动`,
      err
    )
  }

  const text = await res.text()
  const body = text ? safeJson(text) : null

  if (!res.ok && !(opts.accept ?? []).includes(res.status)) {
    const envelope = body as
      | { errorCode?: string; code?: string; message?: string }
      | null
    throw new ApiError(
      res.status,
      envelope?.errorCode ?? envelope?.code ?? `HTTP_${res.status}`,
      envelope?.message ?? text.slice(0, 300) ?? res.statusText,
      body
    )
  }
  return body as T
}

function safeJson(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return null
  }
}

const platform = <T>(
  path: string,
  init?: RequestInit,
  opts?: RequestOptions
): Promise<T> => request<T>(PLATFORM_BASE, path, init, opts)

const json = (v: unknown): RequestInit => ({
  method: "POST",
  body: JSON.stringify(v),
})

// ---------------------------------------------------------------- Business endpoints

export const api = {
  facts: {
    list: () => platform<FactCatalogResponse>("/api/v1/facts"),
  },

  rulesets: {
    list: () => platform<RulesetListResponse>("/api/v1/rulesets"),
    get: (id: string) =>
      platform<RulesetDefinition>(`/api/v1/rulesets/${encodeURIComponent(id)}`),
    /** dry-run: if definition is omitted, the one stored in the library is validated */
    validate: (id: string, definition?: RulesetDefinition) =>
      platform<ValidationReport>(
        `/api/v1/rulesets/${encodeURIComponent(id)}/validate`,
        json(definition ? { definition } : {})
      ),
    /**
     * Save draft: the backend first runs a dry-run and replies 422 with diagnostics if it fails.
     *
     * Success and failure do **not** return the same shape: 200 is `{report, ruleset}`,
     * 422 is a bare report. Here both are normalized to a report so the caller only has to look at
     * `valid` - otherwise the UI would misread "saved" as a failure.
     */
    save: async (id: string, definition: RulesetDefinition) => {
      const res = await platform<
        ValidationReport | { report?: ValidationReport; ruleset?: RulesetDefinition }
      >(
        `/api/v1/rulesets/${encodeURIComponent(id)}`,
        { method: "PUT", body: JSON.stringify(definition) },
        { accept: [422] }
      )
      const wrapped = res as { report?: ValidationReport }
      return wrapped.report ?? (res as ValidationReport)
    },
  },

  packages: {
    list: () => platform<PackageListResponse>("/api/v1/packages"),
    testcases: (pkg: string, version: string) =>
      platform<GatePreviewResponse>(
        `/api/v1/packages/${encodeURIComponent(pkg)}/versions/${encodeURIComponent(version)}/testcases`
      ),
    publish: (pkg: string, version: string, changeId?: string) =>
      platform<{ version: { packageId: string; packageVersion: string; status: string } }>(
        `/api/v1/packages/${encodeURIComponent(pkg)}/versions/${encodeURIComponent(version)}/publish`,
        json({ changeId: changeId ?? "" })
      ),
  },

  changes: {
    list: () => platform<ChangeListResponse>("/api/v1/changes"),
    get: (id: string) =>
      platform<ChangeDetail>(`/api/v1/changes/${encodeURIComponent(id)}`),
    approve: (id: string, role: string, comment: string) =>
      platform<{ change: ChangeDetail; remaining: number }>(
        `/api/v1/changes/${encodeURIComponent(id)}/approve`,
        json({ role, comment })
      ),
    reject: (id: string, role: string, comment: string) =>
      platform<ChangeDetail>(
        `/api/v1/changes/${encodeURIComponent(id)}/reject`,
        json({ role, comment })
      ),
    matrix: () => platform<ApprovalMatrixResponse>("/api/v1/approval-matrix"),
  },

  logs: {
    list: (limit = 50) =>
      platform<LogListResponse>(`/api/v1/logs?limit=${limit}`),
    stats: () => platform<LogStats>("/api/v1/logs/stats"),
    replay: (id: string) =>
      platform<ReplayResponse>(
        `/api/v1/logs/${encodeURIComponent(id)}/replay`,
        json({})
      ),
  },

  audit: {
    list: (limit = 50) =>
      platform<AuditListResponse>(`/api/v1/audit?limit=${limit}`),
  },

  /**
   * Everything engine-related goes through the platform (`/api/v1/engine`, `/api/v1/trial`).
   *
   * The browser never connects to the engine directly: the engine is an unauthenticated internal
   * service, and enabling CORS on it would expose decision-making to any web page. The platform has
   * exactly one explicit CORS switch, and controlling that one is enough.
   */
  engine: {
    info: () => platform<EngineInfo>("/api/v1/engine", undefined, { timeoutMs: 5000 }),
    trial: (payload: TrialPayload) =>
      platform<TrialResult>("/api/v1/trial", json(payload), { timeoutMs: 15_000 }),
  },

  platformHealth: () =>
    platform<unknown>("/healthz", undefined, { timeoutMs: 3000 }),
}

export interface TrialPayload {
  snapshot: {
    snapshotId: string
    tenantId: string
    scene: string
    factSchemaVersion: string
    facts: Record<string, unknown>
  }
  context: {
    tenantId: string
    scene: string
    requestTime: string
    traceId: string
    rulesetId: string
    actor: { type: string; id: string }
  }
}
