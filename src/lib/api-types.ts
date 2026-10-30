// 后端契约的前端镜像 —— 字段名逐个对齐 `platform/contracts` 与 `crates/gr-contract`。
//
// 为什么单独一份而不是复用 `types.ts`：`types.ts` 是 DSL 编辑器的数据模型
// （照 contracts/01 手写的形状），后端返回的规则包/日志/变更单字段更多、
// 且有 `{type, value}` 形态的类型化字面量。两套混在一起会让「谁该有什么字段」
// 变得说不清，宁可多一个文件也要把「线上返回什么」和「编辑器怎么表示」分开。

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

// ---------------------------------------------------------------- 事实目录

export interface FactRecord {
  fact: string
  displayName?: string
  type: string
  unit?: string
  /** 业务语义说明（后端字段名是 semantics，不是 description） */
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

// ---------------------------------------------------------------- 规则集

/** `{type: "decimal", value: "1000"}` 这种带类型的字面量，后端 IR 里常见 */
export interface TypedLiteral {
  type: string
  value: string | number | boolean | null
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
  /** 场景挂多个规则集时必填，否则门禁报 AMBIGUOUS_SCENE */
  rulesetId?: string
  input: Record<string, unknown>
  expect: { decision: string; reasonCode?: string }
  author: string
  status: "pending_review" | "approved" | "rejected"
}

/** 决策表（`contracts/01` §3.2.4）：行即规则，行序即优先级；`*` 表示该维度通配 */
export interface DecisionTableDef {
  tableId: string
  columns: { fact: string; type?: string }[]
  rows: { conditions: Record<string, unknown>; then: RuleThen }[]
  defaultRow?: RuleThen
}

export interface RulesetDefinition {
  rulesetId: string
  /** 人读名称，可空 */
  name?: string
  scene: string
  version: string
  hitStrategy: "FIRST" | "ALL" | "PRIORITY"
  defaultDecision: string
  outputSchemaId?: string
  outputSchemaVersion?: string
  outputSchema?: unknown
  rules?: Rule[]
  /** 决策表写法（与 rules 二选一；注意字段名是 table，不是 decisionTable） */
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

// ---------------------------------------------------------------- dry-run 校验

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

// ---------------------------------------------------------------- 规则包

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

/** 门禁预演：`GET /packages/{pkg}/versions/{v}/testcases` */
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

// ---------------------------------------------------------------- 变更单 / 审批

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

// ---------------------------------------------------------------- 决策日志 / 审计

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

// ---------------------------------------------------------------- 引擎（决策服务）

/** `POST /v1/trial` 的返回：注意它给出了实际执行的包与版本 */
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
  /** 试跑用的是引擎里**已装载**的包，不是编辑器草稿 —— UI 必须把这个说出来 */
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

/** `GET /api/v1/engine`：平台转述引擎的现状（界面不直连引擎） */
export interface EngineInfo {
  healthy: boolean
  baseUrl: string
  health?: unknown
  packages?: EnginePackagesResponse
  /** 活着但拿不到包清单时才有 */
  error?: string
}

export interface HealthResponse {
  [k: string]: unknown
}
