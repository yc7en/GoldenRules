package gov

import "time"

// 变更单状态。注意它与规则包版本状态（`draft/approved/published/retired`）**不是同一个状态机**：
// 变更单描述「这次改动被批了吗」，版本描述「这个包能不能被加载」。二者通过 `approved` 对齐：
// 变更单全票通过 → 平台把对应版本从 draft 推到 approved（`contracts/04` §6）。
const (
	ChangeSubmitted = "submitted"
	ChangeApproved  = "approved"
	ChangeRejected  = "rejected"
	ChangePublished = "published"
)

// 变更类型（`contracts/04` §11.A 模板）。
const (
	ChangeTypeAdd       = "add"
	ChangeTypeThreshold = "threshold"
	ChangeTypeLogic     = "logic"
	ChangeTypeDelete    = "delete"
)

// 灰度形态（`contracts/06` §5.1）。
const (
	GrayShadow      = "shadow"
	GrayPercentage  = "percentage"
	GrayConditional = "conditional"
)

// 错误码。统一 `GOV_` 前缀，写进 `contracts/04` 附录 D。
const (
	CodeMatrixRejected     = "GOV_MATRIX_REJECTED"
	CodeChangeNotFound     = "GOV_CHANGE_NOT_FOUND"
	CodeChangeStateInvalid = "GOV_CHANGE_STATE_INVALID"
	CodeRoleNotRequired    = "GOV_ROLE_NOT_REQUIRED"
	CodeSelfApproval       = "GOV_SELF_APPROVAL"
	CodeAgentCannotApprove = "GOV_AGENT_CANNOT_APPROVE"
	CodeAgentCannotPublish = "GOV_AGENT_CANNOT_PUBLISH"
	CodeSoDApprovePublish  = "GOV_SOD_APPROVE_PUBLISH"
	CodeSoDSubmitPublish   = "GOV_SOD_SUBMIT_PUBLISH"
	CodeChangeRequired     = "GOV_CHANGE_REQUIRED"
	CodeChangeNotApproved  = "GOV_CHANGE_NOT_APPROVED"
	CodePackageMismatch    = "GOV_PACKAGE_MISMATCH"
	CodeGrayInvalid        = "GOV_GRAY_INVALID"
	CodeGrayStepTooLarge   = "GOV_GRAY_STEP_TOO_LARGE"
	CodeInvalidArgument    = "GOV_INVALID_ARGUMENT"
	CodePublishFailed      = "GOV_PUBLISH_FAILED"
	CodeRollbackFailed     = "GOV_ROLLBACK_FAILED"
)

// Approval 一次审批动作。
//
// `Self` 单独记：自审是被矩阵允许的例外，审计时必须一眼看出来是本人给自己批的。
type Approval struct {
	Actor     string `json:"actor"`
	Role      string `json:"role"`
	Slot      int    `json:"slot"`
	Self      bool   `json:"self"`
	At        string `json:"at"`
	Comment   string `json:"comment,omitempty"`
	RiskLevel string `json:"riskLevel"`
}

// Rejection 否决记录（否决后变更单终态，不能再批）。
type Rejection struct {
	Actor   string `json:"actor"`
	Role    string `json:"role"`
	At      string `json:"at"`
	Comment string `json:"comment,omitempty"`
}

// Change 变更单（`contracts/04` §11.A 的机器形态）。
type Change struct {
	ChangeID       string `json:"changeId"`
	PackageID      string `json:"packageId"`
	PackageVersion string `json:"packageVersion"`
	ChangeType     string `json:"changeType"`
	RiskLevel      string `json:"riskLevel"`

	Reason string `json:"reason"`
	Scope  string `json:"scope,omitempty"`

	Submitter     string `json:"submitter"`
	SubmitterType string `json:"submitterType"`
	Status        string `json:"status"`

	// 审批要求由矩阵算出并**固化在变更单里**：事后改矩阵不能追溯改变一张已提交的单子要几个人批。
	Slots        []Slot     `json:"slots"`
	SelfApproval bool       `json:"selfApproval"`
	Approvals    []Approval `json:"approvals"`
	Rejection    *Rejection `json:"rejection,omitempty"`

	Materials []string `json:"materials,omitempty"`

	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	PublishedAt string `json:"publishedAt,omitempty"`
	PublishedBy string `json:"publishedBy,omitempty"`
}

// Remaining 还没有人占的审批位。
func (c Change) Remaining() []Slot {
	out := make([]Slot, 0, len(c.Slots))
	used := make([]bool, len(c.Slots))
	for _, a := range c.Approvals {
		if a.Slot >= 0 && a.Slot < len(used) {
			used[a.Slot] = true
		}
	}
	for i, s := range c.Slots {
		if !used[i] {
			out = append(out, s)
		}
	}
	return out
}

// Gray 灰度配置：运行时「哪个版本吃多少流量」（`contracts/06` §5）。
type Gray struct {
	PackageID string `json:"packageId"`
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	// Percentage 仅 percentage 模式有意义；shadow 恒为 0（不影响线上决策）。
	Percentage int      `json:"percentage"`
	GrayTags   []string `json:"grayTags,omitempty"`
	// PeakPercentage 历史最高放量：判「L4 禁止一步到 100%」需要它。
	PeakPercentage int        `json:"peakPercentage"`
	UpdatedAt      string     `json:"updatedAt"`
	UpdatedBy      string     `json:"updatedBy"`
	History        []GrayStep `json:"history,omitempty"`
}

// GrayStep 一次放量调整（放量本身也是可审计的配置变更，`contracts/06` §5.2）。
type GrayStep struct {
	At         string `json:"at"`
	By         string `json:"by"`
	Mode       string `json:"mode"`
	Percentage int    `json:"percentage"`
	Reason     string `json:"reason,omitempty"`
}

// AuditEvent 审计事件（`contracts/04` §9）。B9 只落「治理动作」，决策日志是 B10。
type AuditEvent struct {
	EventID        string `json:"eventId"`
	At             string `json:"at"`
	Action         string `json:"action"`
	Actor          string `json:"actor"`
	ActorType      string `json:"actorType"`
	ChangeID       string `json:"changeId,omitempty"`
	PackageID      string `json:"packageId,omitempty"`
	PackageVersion string `json:"packageVersion,omitempty"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
	RiskLevel      string `json:"riskLevel,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// 审计动作名。
const (
	ActionChangeSubmitted   = "CHANGE_SUBMITTED"
	ActionChangeApproved    = "CHANGE_APPROVED"
	ActionChangeRejected    = "CHANGE_REJECTED"
	ActionPackagePublished  = "PACKAGE_PUBLISHED"
	ActionPackageRolledBack = "PACKAGE_ROLLED_BACK"
	ActionGrayUpdated       = "GRAY_UPDATED"
	// B10：日志原文访问与回放也要留痕（谁在什么时候看了谁的原始快照）。
	ActionLogRawAccessed = "LOG_RAW_ACCESSED"
	ActionReplayExecuted = "REPLAY_EXECUTED"
)

// Doc 治理仓库文档（一份 JSON：变更单 + 灰度配置 + 审计事件）。
type Doc struct {
	Changes []Change        `json:"changes"`
	Grays   map[string]Gray `json:"grays"` // key = packageId
	Audits  []AuditEvent    `json:"audits"`
	// Seq 每日流水号：CHG-YYYYMMDD-0001。
	Seq map[string]int `json:"seq"`
}

// Error 治理侧领域错误。HTTP 映射集中在 httpapi，本包只说「哪个规则被违反」。
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// NotFoundError 变更单/灰度配置不存在。
type NotFoundError struct {
	ChangeID  string
	PackageID string
}

func (e *NotFoundError) Error() string {
	if e.ChangeID != "" {
		return "变更单不存在: " + e.ChangeID
	}
	return "规则包没有灰度配置: " + e.PackageID
}

func nowString(t time.Time) string { return t.UTC().Format(time.RFC3339) }
