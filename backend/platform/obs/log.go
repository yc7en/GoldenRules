// Package obs 实现 B10：决策日志存储、脱敏视图、原始快照加密与回放。
//
// 契约依据：`contracts/03` §6（日志字段 A-4 / 脱敏 / 不可变）、§8（回放与分级存储）、
// `contracts/02` §9（脱敏与回放）。
//
// # 两套存储，刻意分开
//
//	logs.json  脱敏视图：可长期保留、可给报表和审计看
//	raw.json   原始快照密文：默认只留 90 天，只能经回放通道解密
//
// 分开放不是为了省空间，是为了让「误发一份日志文件」不等于「泄露全部 PII」。
package obs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// LogEntry 决策日志（`contracts/03` §6.1 的 A-4 字段集合）。
//
// 字段与引擎侧 `DecisionLogEntry`（Rust, camelCase）逐字对齐：平台**不解释**决策结果，
// 只负责存、查、回放比对。用 `json.RawMessage` 透传命中路径与输出，
// 避免平台多一层「理解」而与引擎产生第二套语义。
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

// Key 日志的唯一键：同一 trace 可能重试，但 snapshotId+traceId 不会重复。
func (e LogEntry) Key() string {
	return e.SnapshotID + "@" + e.TraceID
}

// Hash 内容摘要（sha256 over canonical JSON）。日志不可变（`contracts/03` §6.3），
// 存一份摘要让「事后被改过」可被检出；P1 再加签名。
func (e LogEntry) Hash() string {
	body, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RawSnapshotRef 原始快照密文的引用。密文本身放在独立的 raw 文档里。
type RawSnapshotRef struct {
	LogID     string `json:"logId"`
	Algo      string `json:"algo"`
	KeyID     string `json:"keyId"`
	Nonce     string `json:"nonce"`
	ExpiresAt string `json:"expiresAt"`
	SizeBytes int    `json:"sizeBytes"`
	// Ciphertext 只在 raw 文档里出现，logs 文档只留引用（见本包说明的两套存储）。
	// 密文只在 raw 文档里出现；logs 文档与接口响应都用 omitempty 隐去（避免把空串当"有密文"）。
	Ciphertext string `json:"ciphertext,omitempty"`
}

// StoredLog 一条落库的日志：脱敏视图 + 原始快照引用（没有原始快照就是"仅脱敏"）。
type StoredLog struct {
	LogID       string          `json:"logId"`
	Entry       LogEntry        `json:"entry"`
	ContentHash string          `json:"contentHash"`
	ReceivedAt  string          `json:"receivedAt"`
	Raw         *RawSnapshotRef `json:"raw,omitempty"`
}

// LogDoc 脱敏视图文档（logs.json）。
type LogDoc struct {
	Logs []StoredLog    `json:"logs"`
	Seq  map[string]int `json:"seq"`
}

// RawDoc 密文文档（raw.json）。与 logs.json 分离，删除保留期到期条目时不影响日志视图。
type RawDoc struct {
	Blobs map[string]RawSnapshotRef `json:"blobs"` // key = logId
}

// IngestItem 一条待入库日志。
type IngestItem struct {
	Log         LogEntry        `json:"log"`
	RawSnapshot json.RawMessage `json:"rawSnapshot,omitempty"`
}

// Query 查询条件（都为空 = 全量，靠 limit/offset 分页）。
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

// ReplayResult 回放结果（`contracts/03` §8.1）。
type ReplayResult struct {
	LogID  string          `json:"logId"`
	Match  bool            `json:"match"`
	Replay json.RawMessage `json:"replay"`
	Diff   []string        `json:"diff"`
	At     string          `json:"at"`
	Note   string          `json:"note,omitempty"`
}

// 错误码（写进 `contracts/03` 附录 D）。
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

// Error 观测侧领域错误。
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Validate 入口校验：A-4 的必填字段缺一不可，否则这条日志对审计没有价值。
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

// filter 是否命中查询条件。
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

// sortByTimeDesc 最新在前（运维排查问题的默认视角）。
func sortByTimeDesc(items []StoredLog) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Entry.ExecutedAt > items[j].Entry.ExecutedAt
	})
}

func nowString(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func dayKey(t time.Time) string { return t.UTC().Format("20060102") }
