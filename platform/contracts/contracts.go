// Package contracts 是管理平台（Go）侧对共享契约的绑定。
//
// 契约的**唯一裁判是 JSON 序列化格式**（PLAN.md §2 B0）：Rust 侧 gr-contract 与这里
// 各自持有绑定，不共享运行时。`go test ./contracts` 会用同一份 `fixtures/` 做往返校验。
//
// 说明：`Expr` 在管理平台侧是**透传**（RawMessage）——表达式语义只由引擎侧解释，
// 平台只在 B7 做 dry-run 校验时把它交给引擎/校验器，自己不改写结构。
package contracts

import (
	"encoding/json"
	"time"
)

// RulePackage 规则包（contracts/01 §6）：原子发布单元、不可变。
type RulePackage struct {
	PackageID         string          `json:"packageId"`
	PackageVersion    string          `json:"packageVersion"`
	FactSchemaVersion string          `json:"factSchemaVersion"`
	DSLVersion        string          `json:"dslVersion"`
	IRVersion         string          `json:"irVersion"`
	Rulesets          []RuleSet       `json:"rulesets"`
	Tests             []TestCase      `json:"tests,omitempty"`
	OutputSchemas     []OutputSchema  `json:"outputSchemas,omitempty"`
	SlotTable         []SlotDef       `json:"slotTable,omitempty"`
	Signature         string          `json:"signature,omitempty"`
	Metadata          PackageMetadata `json:"metadata"`
}

// PackageMetadata 发布元数据。
type PackageMetadata struct {
	CreatedAt string `json:"createdAt,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	RiskLevel string `json:"riskLevel,omitempty"`
	Changelog string `json:"changelog,omitempty"`
}

// RuleSet 规则集：场景之下、规则之上的模块单位（表达式/决策表只是它的写法）。
type RuleSet struct {
	RulesetID           string    `json:"rulesetId"`
	Version             string    `json:"version,omitempty"`
	Scene               string    `json:"scene"`
	HitStrategy         string    `json:"hitStrategy,omitempty"`
	DefaultDecision     string    `json:"defaultDecision"`
	FallbackDecision    string    `json:"fallbackDecision,omitempty"`
	OutputSchemaID      string    `json:"outputSchemaId,omitempty"`
	OutputSchemaVersion string    `json:"outputSchemaVersion,omitempty"`
	Rules               []Rule    `json:"rules"`
	SlotTable           []SlotDef `json:"slotTable"`
}

// Rule 单条规则。When 为 IR 表达式（透传）。
type Rule struct {
	RuleID    string          `json:"ruleId"`
	Version   string          `json:"version,omitempty"`
	Scene     string          `json:"scene,omitempty"`
	Priority  int64           `json:"priority"`
	RiskLevel string          `json:"riskLevel,omitempty"`
	When      json.RawMessage `json:"when"`
	Then      Then            `json:"then"`
}

// Then 命中后果：决策 + 理由码 + 输出变量。
type Then struct {
	Decision      string   `json:"decision"`
	ReasonCode    string   `json:"reasonCode"`
	ReasonMessage string   `json:"reasonMessage,omitempty"`
	Assign        []Assign `json:"assign,omitempty"`
}

// Assign 输出变量赋值（派生事实，纯函数）。
type Assign struct {
	Var  string          `json:"var"`
	Expr json.RawMessage `json:"expr"`
}

// SlotDef slot 表条目：编译期字段引用解析为数组下标。
type SlotDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	FactVersion string `json:"factVersion,omitempty"`
}

// OutputSchema 场景输出 schema（contracts/03 §5）。
type OutputSchema struct {
	OutputSchemaID      string                 `json:"outputSchemaId"`
	OutputSchemaVersion string                 `json:"outputSchemaVersion"`
	Fields              map[string]OutputField `json:"fields"`
}

// OutputField 输出变量契约。
type OutputField struct {
	Type     string   `json:"type"`
	Values   []string `json:"values,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Nullable bool     `json:"nullable,omitempty"`
}

// TestCase 测试用例（规则包一等成员，contracts/01 §6.1）。
type TestCase struct {
	TestID string `json:"testId"`
	Scene  string `json:"scene"`
	// RulesetID 用例针对的规则集（`contracts/01` §6.1）。
	// 场景挂多个规则集时必填，否则引擎侧门禁会以 AMBIGUOUS_SCENE 拒绝执行。
	RulesetID string                     `json:"rulesetId,omitempty"`
	Input     map[string]json.RawMessage `json:"input"`
	Expect    Expectation                `json:"expect"`
	Author    string                     `json:"author,omitempty"`
	Status    string                     `json:"status,omitempty"`
}

// Expectation 期望的决策结果（业务确认此列）。
type Expectation struct {
	Decision   string                     `json:"decision"`
	ReasonCode string                     `json:"reasonCode"`
	Outputs    map[string]json.RawMessage `json:"outputs,omitempty"`
}

// FactSchema 事实契约（contracts/02 §1）。只存契约，不存取数逻辑。
type FactSchema struct {
	Fact        string          `json:"fact"`
	DisplayName string          `json:"displayName,omitempty"`
	Type        string          `json:"type"`
	Format      string          `json:"format,omitempty"`
	Unit        string          `json:"unit,omitempty"`
	Semantics   string          `json:"semantics,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Nullable    bool            `json:"nullable,omitempty"`
	HasDefault  bool            `json:"hasDefault,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Enum        []string        `json:"enum,omitempty"`
	Freshness   *Freshness      `json:"freshness,omitempty"`
	Consistency string          `json:"consistency,omitempty"`
	Source      string          `json:"source,omitempty"`
	SourceVer   string          `json:"sourceVersion,omitempty"`
	Owner       string          `json:"owner,omitempty"`
	Sensitivity string          `json:"sensitivity,omitempty"`
	Version     string          `json:"version,omitempty"`
	Deprecated  bool            `json:"deprecated,omitempty"`
	ReplacedBy  string          `json:"replacedBy,omitempty"`
	OnMissing   string          `json:"onMissing,omitempty"`
}

// Freshness 时效契约（contracts/02 §5）。
type Freshness struct {
	MaxAgeSec int64  `json:"maxAgeSec"`
	OnStale   string `json:"onStale,omitempty"`
}

// FactCatalog 事实目录（单一真源，contracts/02 §1.1）。
type FactCatalog struct {
	CatalogVersion string       `json:"catalogVersion"`
	Facts          []FactSchema `json:"facts"`
}

// FactSnapshot 输入快照（contracts/02 §3）。
type FactSnapshot struct {
	SnapshotID        string                     `json:"snapshotId"`
	SnapshotVersion   string                     `json:"snapshotVersion,omitempty"`
	TenantID          string                     `json:"tenantId"`
	Scene             string                     `json:"scene"`
	FactSchemaVersion string                     `json:"factSchemaVersion"`
	Facts             map[string]json.RawMessage `json:"facts"`
	FactMeta          json.RawMessage            `json:"factMeta,omitempty"`
	MissingFacts      []MissingFact              `json:"missingFacts,omitempty"`
	DegradedFacts     []DegradedFact             `json:"degradedFacts,omitempty"`
	AssembledAt       string                     `json:"assembledAt,omitempty"`
	AssemblerVersion  string                     `json:"assemblerVersion,omitempty"`
}

// MissingFact 缺失事实（唯一表达方式，contracts/02 §3.2）。
type MissingFact struct {
	Fact     string `json:"fact"`
	Reason   string `json:"reason"`
	Required bool   `json:"required,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// DegradedFact 降级事实。
type DegradedFact struct {
	Fact     string `json:"fact"`
	Reason   string `json:"reason"`
	Fallback string `json:"fallback,omitempty"`
}

// DecisionResult 决策输出（contracts/03 §2）。
type DecisionResult struct {
	ResultVersion       string                     `json:"resultVersion"`
	SnapshotID          string                     `json:"snapshotId"`
	TraceID             string                     `json:"traceId"`
	RulesetID           string                     `json:"rulesetId"`
	RulesetVersion      string                     `json:"rulesetVersion"`
	Decision            string                     `json:"decision"`
	ReasonCode          string                     `json:"reasonCode"`
	ReasonMessage       string                     `json:"reasonMessage,omitempty"`
	HitRules            []HitRule                  `json:"hitRules"`
	EvaluatedRules      int                        `json:"evaluatedRules"`
	Outputs             map[string]json.RawMessage `json:"outputs"`
	OutputSchemaID      string                     `json:"outputSchemaId,omitempty"`
	OutputSchemaVersion string                     `json:"outputSchemaVersion,omitempty"`
	Degraded            bool                       `json:"degraded"`
	DegradedReasons     []DegradedReason           `json:"degradedReasons"`
	ExecutionTimeMs     float64                    `json:"executionTimeMs,omitempty"`
	ExecutedAt          string                     `json:"executedAt,omitempty"`
	EngineVersion       string                     `json:"engineVersion,omitempty"`
	PackageID           string                     `json:"packageId,omitempty"`
	PackageVersion      string                     `json:"packageVersion,omitempty"`
}

// HitRule 命中规则（只列命中，无冗余 matched 字段）。
type HitRule struct {
	RuleID      string `json:"ruleId"`
	RuleVersion string `json:"ruleVersion,omitempty"`
	Priority    int64  `json:"priority"`
	ReasonCode  string `json:"reasonCode"`
	Decision    string `json:"decision,omitempty"`
}

// DegradedReason 降级原因（正交于 decision）。
type DegradedReason struct {
	Fact     string `json:"fact"`
	Reason   string `json:"reason"`
	Fallback string `json:"fallback,omitempty"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ErrorContract 真错误响应（contracts/05 §3.2）。契约违规不走这里，走 REJECT 决策。
type ErrorContract struct {
	ErrorCode string                     `json:"errorCode"`
	Message   string                     `json:"message"`
	TraceID   string                     `json:"traceId,omitempty"`
	Details   map[string]json.RawMessage `json:"details,omitempty"`
}

// GoldenTime 契约里的时间字段统一 RFC3339 UTC（contracts/02 §5）。
const GoldenTimeLayout = time.RFC3339

// DSL / IR / 结果契约版本（`contracts/01` §3.1、§4.1，`contracts/03` §10）。
// 三个版本独立演进，跨语言必须对齐 Rust 侧 `gr_contract` 的同名常量。
const (
	DSLVersion    = "1.0"
	IRVersion     = "1.0"
	ResultVersion = "v1"
)
