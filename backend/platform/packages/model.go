// Package packages 实现 B8：规则包编译、原子发布、不可变版本与回滚。
//
// 契约依据：`contracts/01` §6（规则包结构）、`contracts/06`（生命周期：版本 / 生效时间 /
// 状态机 / 灰度 / 回滚）。本包只管「包这个资产怎么流转」，不关心规则长什么样（`dsl` 包管），
// 也不关心谁批准（B9 的 `gov` 包管）。
//
// # 交付给引擎的产物
//
// 平台冻结的是 **IR + slot 表 + 签名**（`contracts/01` §6 的 `RulePackage`）。
// 字节码是引擎加载期由 IR 派生的（B2 `gr-vm`），**不入包**——`contracts/01` §4.1 明确
// 「IR 是长期资产，执行后端可替换（VM → JIT → WASM）」，把字节码写进包会把执行后端钉死。
package packages

import (
	"time"

	"goldenrules/platform/contracts"
)

// 版本状态机（`contracts/06` §4.1）。
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

// 版本状态相关的错误码。
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
	// F-PKG06 发布门禁
	CodeVerifyFailed = "PKG_TESTCASE_FAILED"
	// CodeVerifyUnavailable 「包里有 approved 用例，但没有执行器能证明它们通过」。
	// 这时**不能放行**：放行等于把「已验证」变成一句没人核实过的话。
	CodeVerifyUnavailable = "PKG_VERIFY_UNAVAILABLE"
)

// Version 一个规则包版本。发布后**内容不可变**（`contracts/06` §2.2），
// 只有状态与生效时间这些「生命周期字段」允许变化。
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

	// 生命周期时间（UTC）。`validFrom` 之前不生效，`validUntil` 到期退役。
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

	// 用例计数：`contracts/01` §6.1 要求发布时显式提示未确认用例数，
	// B9 的门禁（approved 用例必须全绿才能发布）也读这两个字段。
	UnconfirmedTC int `json:"unconfirmedTestCases"`
	ConfirmedTC   int `json:"confirmedTestCases"`

	// 包内容（IR 形态）。发布后与 Signature 一起参与完整性校验。
	Package contracts.RulePackage `json:"package"`
}

// Repo 包仓库文档。`Active` 是「哪个版本正在生效」的指针（`contracts/06` §6）。
type Repo struct {
	Versions []Version         `json:"versions"`
	Active   map[string]string `json:"active"`
}

// ensureActive 保证 Active 指针表可写。
//
// persist 的 init 回调由调用方给，可能只给了 `Repo{}`；JSON 里字段缺省时 Unmarshal 也会
// 把 map 留成 nil。往 nil map 写会 panic，所以在所有写入点前统一兜一层。
func (r *Repo) ensureActive() {
	if r.Active == nil {
		r.Active = map[string]string{}
	}
}

// Clone 深拷贝（persist 的回滚靠它，不能只拷结构体）。
func (v Version) Clone() Version {
	out := v
	out.SourceHashes = make(map[string]string, len(v.SourceHashes))
	for k, val := range v.SourceHashes {
		out.SourceHashes[k] = val
	}
	out.RulesetIDs = append([]string(nil), v.RulesetIDs...)
	return out
}

// IsEffective 判断某版本在给定时刻是否生效（`contracts/06` §3）。
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

// NotFoundError 版本或包不存在。
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

// StateError 状态机拒绝。
type StateError struct {
	Code    string
	Message string
}

func (e *StateError) Error() string { return e.Message }
