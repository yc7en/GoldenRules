// Package contracts is the management platform's (Go) binding of the shared contracts.
//
// The **only referee for the contract is the JSON serialisation format** (PLAN.md §2 B0): the Rust-side gr-contract and this package
// each hold their own binding and share no runtime. `go test ./contracts` round-trips against the same `fixtures/`.
//
// Note: on the management platform side `Expr` is a **pass-through** (RawMessage) — expression semantics are interpreted only by the engine side;
// the platform only hands it to the engine/validator during B7 dry-run validation and never rewrites the structure itself.
package contracts

import (
	"encoding/json"
	"time"
)

// RulePackage is a rule package (contracts/01 §6): the atomic publish unit, immutable.
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

// PackageMetadata is the publish metadata.
type PackageMetadata struct {
	CreatedAt string `json:"createdAt,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	RiskLevel string `json:"riskLevel,omitempty"`
	Changelog string `json:"changelog,omitempty"`
}

// RuleSet is a ruleset: the unit of modularity below a scene and above rules (expressions/decision tables are merely ways to write it).
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

// Rule is a single rule. When is an IR expression (pass-through).
type Rule struct {
	RuleID    string          `json:"ruleId"`
	Version   string          `json:"version,omitempty"`
	Scene     string          `json:"scene,omitempty"`
	Priority  int64           `json:"priority"`
	RiskLevel string          `json:"riskLevel,omitempty"`
	When      json.RawMessage `json:"when"`
	Then      Then            `json:"then"`
}

// Then is the hit consequence: decision + reason code + output variables.
type Then struct {
	Decision      string   `json:"decision"`
	ReasonCode    string   `json:"reasonCode"`
	ReasonMessage string   `json:"reasonMessage,omitempty"`
	Assign        []Assign `json:"assign,omitempty"`
}

// Assign is an output variable assignment (a derived fact, pure function).
type Assign struct {
	Var  string          `json:"var"`
	Expr json.RawMessage `json:"expr"`
}

// SlotDef is a slot table entry: field references are resolved into array indices at compile time.
type SlotDef struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	FactVersion string `json:"factVersion,omitempty"`
}

// OutputSchema is the scene output schema (contracts/03 §5).
type OutputSchema struct {
	OutputSchemaID      string                 `json:"outputSchemaId"`
	OutputSchemaVersion string                 `json:"outputSchemaVersion"`
	Fields              map[string]OutputField `json:"fields"`
}

// OutputField is an output variable contract.
type OutputField struct {
	Type     string   `json:"type"`
	Values   []string `json:"values,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Nullable bool     `json:"nullable,omitempty"`
}

// TestCase is a test case (a first-class member of the rule package, contracts/01 §6.1).
type TestCase struct {
	TestID string `json:"testId"`
	Scene  string `json:"scene"`
	// RulesetID is the ruleset this test case targets (`contracts/01` §6.1).
	// Required when a scene carries several rulesets, otherwise the engine-side gate refuses to run with AMBIGUOUS_SCENE.
	RulesetID string                     `json:"rulesetId,omitempty"`
	Input     map[string]json.RawMessage `json:"input"`
	Expect    Expectation                `json:"expect"`
	Author    string                     `json:"author,omitempty"`
	Status    string                     `json:"status,omitempty"`
}

// Expectation is the expected decision result (business confirms this column).
type Expectation struct {
	Decision   string                     `json:"decision"`
	ReasonCode string                     `json:"reasonCode"`
	Outputs    map[string]json.RawMessage `json:"outputs,omitempty"`
}

// FactSchema is a fact contract (contracts/02 §1). It stores contracts only, never fetch logic.
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

// Freshness is the freshness contract (contracts/02 §5).
type Freshness struct {
	MaxAgeSec int64  `json:"maxAgeSec"`
	OnStale   string `json:"onStale,omitempty"`
}

// FactCatalog is the fact catalog (single source of truth, contracts/02 §1.1).
type FactCatalog struct {
	CatalogVersion string       `json:"catalogVersion"`
	Facts          []FactSchema `json:"facts"`
}

// FactSnapshot is the input snapshot (contracts/02 §3).
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

// MissingFact is a missing fact (the only way to express it, contracts/02 §3.2).
type MissingFact struct {
	Fact     string `json:"fact"`
	Reason   string `json:"reason"`
	Required bool   `json:"required,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// DegradedFact is a degraded fact.
type DegradedFact struct {
	Fact     string `json:"fact"`
	Reason   string `json:"reason"`
	Fallback string `json:"fallback,omitempty"`
}

// DecisionResult is the decision output (contracts/03 §2).
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

// HitRule is a hit rule (only hits are listed; no redundant matched field).
type HitRule struct {
	RuleID      string `json:"ruleId"`
	RuleVersion string `json:"ruleVersion,omitempty"`
	Priority    int64  `json:"priority"`
	ReasonCode  string `json:"reasonCode"`
	Decision    string `json:"decision,omitempty"`
}

// DegradedReason is a degrade reason (orthogonal to decision).
type DegradedReason struct {
	Fact     string `json:"fact"`
	Reason   string `json:"reason"`
	Fallback string `json:"fallback,omitempty"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ErrorContract is a true error response (contracts/05 §3.2). Contract violations do not come here; they become REJECT decisions.
type ErrorContract struct {
	ErrorCode string                     `json:"errorCode"`
	Message   string                     `json:"message"`
	TraceID   string                     `json:"traceId,omitempty"`
	Details   map[string]json.RawMessage `json:"details,omitempty"`
}

// GoldenTime: time fields in the contract are uniformly RFC3339 UTC (contracts/02 §5).
const GoldenTimeLayout = time.RFC3339

// DSL / IR / result contract versions (`contracts/01` §3.1, §4.1, `contracts/03` §10).
// The three versions evolve independently and must stay aligned with the same-named constants in the Rust-side `gr_contract`.
const (
	DSLVersion    = "1.0"
	IRVersion     = "1.0"
	ResultVersion = "v1"
)
