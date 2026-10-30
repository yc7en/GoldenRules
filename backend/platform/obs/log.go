// Package obs implements B10: decision log storage, redacted views, raw snapshot encryption and replay.
//
// Contract basis: `contracts/03` §6 (log fields A-4 / redaction / immutability), §8 (replay and tiered storage),
// `contracts/02` §9 (redaction and replay).
//
// # Two stores, deliberately separate
//
//	logs.json  redacted view: kept long term, readable by reports and audit
//	raw.json   raw snapshot ciphertext: kept 90 days by default, decryptable only through the replay channel
//
// Keeping them apart is not about saving space; it is so that "one log file sent to the wrong place" does not equal "all PII leaked".
package obs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// LogEntry is a decision log (the A-4 field set of `contracts/03` §6.1).
//
// The fields are aligned word for word with the engine-side `DecisionLogEntry` (Rust, camelCase): the platform **does not interpret** decision results,
// it only stores, queries and replays them for comparison. `json.RawMessage` passes the hit path and outputs through,
// avoiding an extra layer of "understanding" that would give the platform a second semantics alongside the engine.
type LogEntry struct {
	SnapshotID        string                     `json:"snapshotId"`
	TraceID           string                     `json:"traceId"`
	TenantID          string                     `json:"tenantId"`
	Scene             string                     `json:"scene"`
	FactSchemaVersion string                     `json:"factSchemaVersion"`
	PackageID         string                     `json:"packageId"`
	PackageVersion    string                     `json:"packageVersion"`
	RulesetID         string                     `json:"rulesetId"`
	RulesetVersion    string                     `json:"rulesetVersion"`
	MaskedFacts       map[string]json.RawMessage `json:"maskedFacts"`
	MaskedFields      []string                   `json:"maskedFields"`
	HitRules          []json.RawMessage          `json:"hitRules"`
	Outputs           map[string]json.RawMessage `json:"outputs"`
	Decision          string                     `json:"decision"`
	ReasonCode        string                     `json:"reasonCode"`
	Degraded          bool                       `json:"degraded"`
	DegradedReasons   []json.RawMessage          `json:"degradedReasons"`
	EngineVersion     string                     `json:"engineVersion"`
	ExecutionTimeMs   float64                    `json:"executionTimeMs"`
	EvaluatedRules    int                        `json:"evaluatedRules"`
	ExecutedAt        string                     `json:"executedAt"`
}

// Key is the unique key of a log: the same trace may be retried, but snapshotId+traceId never repeat.
func (e LogEntry) Key() string {
	return e.SnapshotID + "@" + e.TraceID
}

// Hash is the content digest (sha256 over canonical JSON). Logs are immutable (`contracts/03` §6.3),
// so storing a digest makes "changed after the fact" detectable; signing comes in P1.
func (e LogEntry) Hash() string {
	body, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RawSnapshotRef is a reference to the raw snapshot ciphertext. The ciphertext itself lives in the separate raw document.
type RawSnapshotRef struct {
	LogID     string `json:"logId"`
	Algo      string `json:"algo"`
	KeyID     string `json:"keyId"`
	Nonce     string `json:"nonce"`
	ExpiresAt string `json:"expiresAt"`
	SizeBytes int    `json:"sizeBytes"`
	// Ciphertext only appears in the raw document; the logs document keeps only the reference (see the two stores described in this package).
	// The ciphertext only appears in the raw document; the logs document and API responses hide it via omitempty (so an empty string is not read as "has ciphertext").
	Ciphertext string `json:"ciphertext,omitempty"`
}

// StoredLog is one stored log: the redacted view + the raw snapshot reference (no raw snapshot means "redacted only").
type StoredLog struct {
	LogID       string          `json:"logId"`
	Entry       LogEntry        `json:"entry"`
	ContentHash string          `json:"contentHash"`
	ReceivedAt  string          `json:"receivedAt"`
	Raw         *RawSnapshotRef `json:"raw,omitempty"`
}

// LogDoc is the redacted-view document (logs.json).
type LogDoc struct {
	Logs []StoredLog    `json:"logs"`
	Seq  map[string]int `json:"seq"`
}

// RawDoc is the ciphertext document (raw.json). It is separate from logs.json, so purging expired entries does not touch the log view.
type RawDoc struct {
	Blobs map[string]RawSnapshotRef `json:"blobs"` // key = logId
}

// IngestItem is one log waiting to be stored.
type IngestItem struct {
	Log         LogEntry        `json:"log"`
	RawSnapshot json.RawMessage `json:"rawSnapshot,omitempty"`
}

// Query holds the query conditions (all empty = everything, paged via limit/offset).
type Query struct {
	TenantID       string
	Scene          string
	Decision       string
	ReasonCode     string
	PackageVersion string
	Degraded       *bool
	From           string
	To             string
	Limit          int
	Offset         int
}

// ReplayResult is the replay result (`contracts/03` §8.1).
type ReplayResult struct {
	LogID  string          `json:"logId"`
	Match  bool            `json:"match"`
	Replay json.RawMessage `json:"replay"`
	Diff   []string        `json:"diff"`
	At     string          `json:"at"`
	Note   string          `json:"note,omitempty"`
}

// Error codes (written into `contracts/03` Appendix D).
const (
	CodeLogNotFound       = "OBS_LOG_NOT_FOUND"
	CodeInvalidLog        = "OBS_LOG_INVALID"
	CodeDuplicateLog      = "OBS_LOG_DUPLICATE"
	CodeRawUnavailable    = "OBS_RAW_UNAVAILABLE"
	CodeRawPurged         = "OBS_RAW_PURGED"
	CodeKeyMissing        = "OBS_KEY_MISSING"
	CodeKeyUnknown        = "OBS_KEY_UNKNOWN"
	CodeKeyInvalid        = "OBS_KEY_INVALID"
	CodeDecryptFailed     = "OBS_DECRYPT_FAILED"
	CodeReplayUnavailable = "OBS_REPLAY_UNAVAILABLE"
	CodeReplayFailed      = "OBS_REPLAY_FAILED"
)

// Error is an observability-side domain error.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Validate is the entry-point validation: every required A-4 field must be present, otherwise the log is worthless for audit.
func (e LogEntry) Validate() error {
	missing := make([]string, 0, 6)
	for _, f := range []struct {
		name string
		val  string
	}{
		{"snapshotId", e.SnapshotID},
		{"traceId", e.TraceID},
		{"tenantId", e.TenantID},
		{"scene", e.Scene},
		{"decision", e.Decision},
		{"executedAt", e.ExecutedAt},
	} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return errf(CodeInvalidLog, "决策日志缺必填字段（A-4）："+strings.Join(missing, ", "))
	}
	if _, err := time.Parse(time.RFC3339, e.ExecutedAt); err != nil {
		return errf(CodeInvalidLog, "executedAt 必须是 RFC3339："+e.ExecutedAt)
	}
	return nil
}

// filter reports whether the entry matches the query conditions.
func (q Query) filter(e LogEntry) bool {
	if q.TenantID != "" && e.TenantID != q.TenantID {
		return false
	}
	if q.Scene != "" && e.Scene != q.Scene {
		return false
	}
	if q.Decision != "" && e.Decision != q.Decision {
		return false
	}
	if q.ReasonCode != "" && e.ReasonCode != q.ReasonCode {
		return false
	}
	if q.PackageVersion != "" && e.PackageVersion != q.PackageVersion {
		return false
	}
	if q.Degraded != nil && e.Degraded != *q.Degraded {
		return false
	}
	if q.From != "" && e.ExecutedAt < q.From {
		return false
	}
	if q.To != "" && e.ExecutedAt > q.To {
		return false
	}
	return true
}

// sortByTimeDesc puts the newest first (the default view for ops troubleshooting).
func sortByTimeDesc(items []StoredLog) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Entry.ExecutedAt > items[j].Entry.ExecutedAt
	})
}

func nowString(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func dayKey(t time.Time) string { return t.UTC().Format("20060102") }
