// Frontend mirror of the backend contract - field names align one by one with `platform/contracts` and `crates/gr-contract`.
//
// Why a separate file instead of reusing `types.ts`: `types.ts` is the data model of the DSL editor
// (shaped by hand after contracts/01), while what the backend returns for rulesets / logs / change
// requests has more fields and carries `{type, value}` typed literals. Mixing the two makes "who should
// have which field" impossible to state clearly, so an extra file is preferable to blurring "what the
// wire returns" and "how the editor represents it".

export type RiskLevel = "L1" | "L2" | "L3" | "L4"
export type ActorType =
  | "engineer"
  | "business"
  | "risk"
  | "compliance"
  | "owner"
  | "peer"
  | "agent"
  | "user"

// ---------------------------------------------------------------- Fact catalog

export interface FactRecord {
  fact: string
  displayName?: string
  type: string
  unit?: string
  /** Business semantics note (the backend field is named semantics, not description) */
  semantics?: string
  required: boolean
  nullable?: boolean
  enum?: string[]
  consistency?: string
  owner?: string
  sensitivity?: string
  version?: string
  onMissing?: string
  hasDefault?: boolean
  default?: unknown
  source?: string
  deprecated?: boolean
  deprecatedAt?: string
}

export interface FactCatalogResponse {
  catalogVersion: string
  total?: number
  items: FactRecord[]
}

// ---------------------------------------------------------------- Rulesets

/** Typed literals like `{type: "decimal", value: "1000"}`, common in the backend IR */
export interface TypedLiteral {
  type: string
  /** `array` literals are the right-hand side of `in` / `notIn`, so the value is not always scalar. */
  value: string | number | boolean | null | (string | number | boolean)[]
}

export type ExprNode =
  | { fact: string }
  | { op: string; args: ExprNode[] }
  | TypedLiteral
  | string
  | number
  | boolean
  | null

export interface AssignEntry {
  var: string
  expr: ExprNode
}

export interface RuleThen {
  decision: string
  reasonCode: string
  assign?: AssignEntry[]
}

export interface Rule {
  ruleId: string
  version: string
  scene: string
  priority: number
  riskLevel: RiskLevel
  when: ExprNode
  then: RuleThen
  enabled?: boolean
}

export interface TestCaseRecord {
  testId: string
  scene: string
  /** Required when a scene has more than one ruleset attached, otherwise the gate reports AMBIGUOUS_SCENE */
  rulesetId?: string
  input: Record<string, unknown>
  expect: { decision: string; reasonCode?: string }
  author: string
  status: "pending_review" | "approved" | "rejected"
}

/** Decision table (`contracts/01` §3.2.4): a row is a rule, row order is priority; `*` means wildcard for that dimension */
export interface DecisionTableDef {
  tableId: string
  columns: { fact: string; type?: string }[]
  rows: { conditions: Record<string, unknown>; then: RuleThen }[]
  defaultRow?: RuleThen
}

export interface RulesetDefinition {
  rulesetId: string
  /** Human-readable name, optional */
  name?: string
  scene: string
  version: string
  hitStrategy: "FIRST" | "ALL" | "PRIORITY"
  defaultDecision: string
  outputSchemaId?: string
  outputSchemaVersion?: string
  outputSchema?: unknown
  rules?: Rule[]
  /** Decision table form (either this or rules; note the field is named table, not decisionTable) */
  table?: DecisionTableDef
  tests?: TestCaseRecord[]
  updatedAt?: string
  updatedBy?: string
  revision?: number
}

export interface RulesetListResponse {
  total?: number
  items: RulesetDefinition[]
}

// ---------------------------------------------------------------- dry-run validation

export interface ValidationIssue {
  code: string
  level: "error" | "warn"
  path: string
  ruleId?: string
  fact?: string
  message: string
  suggest?: string
}

export interface ValidationReport {
  rulesetId: string
  valid: boolean
  errors: ValidationIssue[]
  warnings: ValidationIssue[]
  stats?: {
    rules: number
    slots: number
    maxDepth: number
    maxNodes: number
    assignCount: number
  }
  ir?: unknown
}

// ---------------------------------------------------------------- Ruleset packages

export type PackageStatus =
  | "draft"
  | "approved"
  | "published"
  | "retired"
  | "rejected"

export interface PackageVersionSummary {
  packageId: string
  packageVersion: string
  status: PackageStatus
  riskLevel?: RiskLevel
  rulesetIds?: string[]
  ruleCount?: number
  signature?: string
  factSchemaVersion?: string
  createdAt?: string
  createdBy?: string
  publishedAt?: string
  publishedBy?: string
  retiredAt?: string
  retiredCause?: string
  supersededBy?: string
  validFrom?: string
  validUntil?: string
  confirmedTestCases?: number
  unconfirmedTestCases?: number
}

export interface PackageListResponse {
  total?: number
  items: PackageVersionSummary[]
}

/** Gate preview: `GET /packages/{pkg}/versions/{v}/testcases` */
export interface GatePreviewResponse {
  gate: {
    approvedCases: number
    unconfirmedCases: number
    enforcedOnPublish: boolean
    note?: string
  }
  report: {
    passed: boolean
    summary: string
    total: number
    ran: number
    passedCount: number
    failedCount: number
    skipped: number
    backend?: string
    failures: { testId: string; code: string; message: string }[]
  }
}

// ---------------------------------------------------------------- Change requests / approval

export interface SlaInfo {
  riskLevel: RiskLevel
  applicable: boolean
  targetHours: number
  escalateAfterHours: number
  deadlineAt: string
  escalateAt: string
  elapsedHours: number
  overdue: boolean
  escalated: boolean
  escalateTo: string
}

export interface ChangeSummary {
  changeId: string
  packageId: string
  packageVersion: string
  changeType: string
  riskLevel: RiskLevel
  status: "submitted" | "approval" | "approved" | "rejected" | "published" | "draft"
  submitter: string
  submitterType: string
  required: number
  approved: number
  remaining: number
  sla?: SlaInfo
  createdAt: string
  reason?: string
}

export interface ApprovalSlot {
  roles: string[]
  label: string
}

export interface ApprovalRecord {
  actor: string
  role: string
  slot: number
  self: boolean
  at: string
  comment?: string
}

export interface ChangeDetail extends ChangeSummary {
  scope?: string
  slots: ApprovalSlot[]
  selfApproval?: boolean
  approvals: ApprovalRecord[]
  updatedAt?: string
  publishedAt?: string
  publishedBy?: string
  emergency?: boolean
  emergencyBy?: string
  makeupReviewDueAt?: string
}

export interface ChangeListResponse {
  total?: number
  items: ChangeSummary[]
}

export interface MatrixCell {
  riskLevel: RiskLevel
  submitterType: string
  requiredCount: number
  slotLabels: string[]
  slotRoles: string[][]
  forbidden: boolean
  forbiddenCause?: string
  selfApproval?: boolean
}

export interface ApprovalMatrixResponse {
  cells: MatrixCell[]
}

// ---------------------------------------------------------------- Decision logs / audit

export interface DecisionLogEntry {
  snapshotId: string
  traceId?: string
  tenantId?: string
  scene: string
  packageId?: string
  packageVersion?: string
  rulesetId?: string
  rulesetVersion?: string
  maskedFacts: Record<string, unknown>
  maskedFields?: string[]
  hitRules?: { ruleId: string; ruleVersion?: string }[]
  outputs?: Record<string, unknown>
  decision: string
  reasonCode: string
  degraded: boolean
  degradedReasons?: string[]
  engineVersion?: string
  executionTimeMs?: number
  evaluatedRules?: number
  executedAt: string
}

export interface LogItem {
  logId: string
  entry: DecisionLogEntry
  contentHash?: string
  receivedAt?: string
}

export interface LogListResponse {
  total?: number
  items: LogItem[]
  storage?: {
    rawEnabled?: boolean
    replayAvailable?: boolean
    backend?: string
    retentionDays?: number
  }
}

export interface LogStats {
  total: number
  byDecision: Record<string, number>
  byScene: Record<string, number>
  degraded: number
  avgLatencyMs: number
  maxLatencyMs: number
  rawEnabled: boolean
  retentionDays: number
}

export interface ReplayResponse {
  result: {
    match: boolean
    reason?: string
    replay?: { decision: string; reasonCode: string }
    original?: { decision: string; reasonCode: string }
  }
}

export interface AuditEvent {
  eventId: string
  at: string
  action: string
  actor: string
  actorType: string
  changeId?: string
  packageId?: string
  packageVersion?: string
  from?: string
  to?: string
  riskLevel?: string
  reason?: string
  detail?: string
}

export interface AuditListResponse {
  total?: number
  items: AuditEvent[]
}

// ---------------------------------------------------------------- Engine (decision service)

/** Return of `POST /v1/trial`: note that it reports the package and version actually executed */
export interface TrialResult {
  resultVersion?: string
  snapshotId: string
  traceId?: string
  rulesetId: string
  rulesetVersion: string
  decision: string
  reasonCode: string
  hitRules: {
    ruleId: string
    ruleVersion?: string
    priority?: number
    reasonCode?: string
    decision?: string
  }[]
  evaluatedRules: number
  outputs: Record<string, unknown>
  outputSchemaId?: string
  outputSchemaVersion?: string
  degraded: boolean
  degradedReasons: string[]
  executionTimeMs: number
  executedAt?: string
  engineVersion?: string
  /** A trial run uses the package **already loaded** in the engine, not the editor draft - the UI must say so */
  packageId: string
  packageVersion: string
}

export interface EnginePackagesResponse {
  packages: {
    packageId: string
    packageVersion: string
    rules: number
    rulesets: number
    instrs?: number
    estBytes?: number
  }[]
}

/** `GET /api/v1/engine`: the platform relays the engine's current state (the UI never talks to the engine directly) */
export interface EngineInfo {
  healthy: boolean
  baseUrl: string
  health?: unknown
  packages?: EnginePackagesResponse
  /** Only present when it is alive but the package list cannot be fetched */
  error?: string
}

export interface HealthResponse {
  [k: string]: unknown
}
