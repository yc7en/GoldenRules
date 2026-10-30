// Package packages implements B8: rule package compilation, atomic publish, immutable versions and rollback.
//
// Contract basis: `contracts/01` §6 (rule package structure), `contracts/06` (lifecycle: versions / effective time /
// state machine / canary / rollback). This package only cares about "how the package asset moves", not what rules look like (the `dsl` package owns that),
// nor who approves (B9's `gov` package owns that).
//
// # Artifact delivered to the engine
//
// What the platform freezes is the **IR + slot table + signature** (`RulePackage` of `contracts/01` §6).
// Bytecode is derived from the IR at engine load time (B2 `gr-vm`) and **is not part of the package** — `contracts/01` §4.1 states clearly that
// "the IR is the long-lived asset and the execution backend is replaceable (VM → JIT → WASM)"; writing bytecode into the package would pin the backend.
package packages

import (
	"time"

	"goldenrules/platform/contracts"
)

// Version state machine (`contracts/06` §4.1).
//
//	draft → approved → published → retired
//	              ↓
//	           rejected
const (
	StatusDraft     = "draft"
	StatusApproved  = "approved"
	StatusPublished = "published"
	StatusRetired   = "retired"
	StatusRejected  = "rejected"
)

// Error codes related to version state.
const (
	CodeNotFound       = "PKG_NOT_FOUND"
	CodeImmutable      = "PKG_IMMUTABLE"
	CodeBadTransition  = "PKG_BAD_TRANSITION"
	CodeCompileFailed  = "PKG_COMPILE_FAILED"
	CodeSignatureBad   = "PKG_SIGNATURE_INVALID"
	CodeNoActive       = "PKG_NO_ACTIVE_VERSION"
	CodeVersionExists  = "PKG_VERSION_EXISTS"
	CodeEmptyPackage   = "PKG_EMPTY"
	CodeNotApproved    = "PKG_NOT_APPROVED"
	CodeStillServing   = "PKG_STILL_SERVING"
	CodeRollbackTarget = "PKG_ROLLBACK_TARGET_INVALID"
	// F-PKG06 publish gate
	CodeVerifyFailed = "PKG_TESTCASE_FAILED"
	// CodeVerifyUnavailable means "the package has approved test cases, but no executor can prove they pass".
	// It must **not** be let through: letting it through turns "verified" into a claim nobody ever checked.
	CodeVerifyUnavailable = "PKG_VERIFY_UNAVAILABLE"
)

// Version is one rule package version. After publish the **content is immutable** (`contracts/06` §2.2);
// only the lifecycle fields such as status and effective time may change.
type Version struct {
	PackageID         string `json:"packageId"`
	PackageVersion    string `json:"packageVersion"`
	FactSchemaVersion string `json:"factSchemaVersion"`
	DSLVersion        string `json:"dslVersion"`
	IRVersion         string `json:"irVersion"`
	Status            string `json:"status"`

	RiskLevel string `json:"riskLevel,omitempty"`
	Changelog string `json:"changelog,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`

	// Lifecycle times (UTC). Not effective before `validFrom`; retired when `validUntil` expires.
	ValidFrom  string `json:"validFrom,omitempty"`
	ValidUntil string `json:"validUntil,omitempty"`

	PublishedAt  string `json:"publishedAt,omitempty"`
	PublishedBy  string `json:"publishedBy,omitempty"`
	RetiredAt    string `json:"retiredAt,omitempty"`
	RetiredCause string `json:"retiredCause,omitempty"`
	SupersededBy string `json:"supersededBy,omitempty"`

	Signature    string            `json:"signature"`
	RuleCount    int               `json:"ruleCount"`
	SourceHashes map[string]string `json:"sourceHashes"`
	RulesetIDs   []string          `json:"rulesetIds"`

	// Test case counts: `contracts/01` §6.1 requires publish to warn explicitly about the number of unconfirmed test cases,
	// and B9's gate (approved test cases must all be green before publish) reads these two fields as well.
	UnconfirmedTC int `json:"unconfirmedTestCases"`
	ConfirmedTC   int `json:"confirmedTestCases"`

	// Package content (IR form). After publish it participates in the integrity check together with Signature.
	Package contracts.RulePackage `json:"package"`
}

// Repo is the package repository document. `Active` is the pointer to "which version is currently effective" (`contracts/06` §6).
type Repo struct {
	Versions []Version         `json:"versions"`
	Active   map[string]string `json:"active"`
}

// ensureActive makes sure the Active pointer table is writable.
//
// The persist init callback is supplied by the caller, who may pass just `Repo{}`; Unmarshal also leaves the map
// as nil when the JSON field is absent. Writing to a nil map panics, so every write site gets the same guard.
func (r *Repo) ensureActive() {
	if r.Active == nil {
		r.Active = map[string]string{}
	}
}

// Clone deep-copies (persist's rollback relies on it; copying the struct alone is not enough).
func (v Version) Clone() Version {
	out := v
	out.SourceHashes = make(map[string]string, len(v.SourceHashes))
	for k, val := range v.SourceHashes {
		out.SourceHashes[k] = val
	}
	out.RulesetIDs = append([]string(nil), v.RulesetIDs...)
	return out
}

// IsEffective reports whether a version is effective at the given moment (`contracts/06` §3).
func (v Version) IsEffective(now time.Time) bool {
	if v.Status != StatusPublished {
		return false
	}
	if v.ValidFrom != "" {
		if t, err := time.Parse(time.RFC3339, v.ValidFrom); err == nil && now.Before(t) {
			return false
		}
	}
	if v.ValidUntil != "" {
		if t, err := time.Parse(time.RFC3339, v.ValidUntil); err == nil && !now.Before(t) {
			return false
		}
	}
	return true
}

// NotFoundError means the version or package does not exist.
type NotFoundError struct {
	PackageID string
	Version   string
}

func (e *NotFoundError) Error() string {
	if e.Version == "" {
		return "规则包不存在: " + e.PackageID
	}
	return "规则包版本不存在: " + e.PackageID + "@" + e.Version
}

// StateError is a state machine refusal.
type StateError struct {
	Code    string
	Message string
}

func (e *StateError) Error() string { return e.Message }
