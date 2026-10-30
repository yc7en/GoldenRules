package gov

import "time"

// Change request states. Note that this is **not the same state machine** as the rule package version states (`draft/approved/published/retired`):
// a change request describes "has this change been approved", a version describes "can this package be loaded". The two align through `approved`:
// all slots of the change request pass → the platform moves the corresponding version from draft to approved (`contracts/04` §6).
const (
	ChangeSubmitted = "submitted"
	ChangeApproved  = "approved"
	ChangeRejected  = "rejected"
	ChangePublished = "published"
)

// Change types (`contracts/04` §11.A template).
const (
	ChangeTypeAdd       = "add"
	ChangeTypeThreshold = "threshold"
	ChangeTypeLogic     = "logic"
	ChangeTypeDelete    = "delete"
)

// Canary modes (`contracts/06` §5.1).
const (
	GrayShadow      = "shadow"
	GrayPercentage  = "percentage"
	GrayConditional = "conditional"
)

// Error codes. All share the `GOV_` prefix and are documented in `contracts/04` appendix D.
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

// Approval is one approval action.
//
// `Self` is recorded separately: self-approval is an exception the matrix allows, so an audit must make it obvious at a glance that someone approved their own change.
type Approval struct {
	Actor     string `json:"actor"`
	Role      string `json:"role"`
	Slot      int    `json:"slot"`
	Self      bool   `json:"self"`
	At        string `json:"at"`
	Comment   string `json:"comment,omitempty"`
	RiskLevel string `json:"riskLevel"`
}

// Rejection is the record of a refusal (after a refusal the change request is final and can no longer be approved).
type Rejection struct {
	Actor   string `json:"actor"`
	Role    string `json:"role"`
	At      string `json:"at"`
	Comment string `json:"comment,omitempty"`
}

// Change is a change request (the machine form of `contracts/04` §11.A).
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

	// The approval requirement is computed from the matrix and **frozen into the change request**: changing the matrix later must not retroactively alter how many approvers an already-submitted request needs.
	Slots        []Slot     `json:"slots"`
	SelfApproval bool       `json:"selfApproval"`
	Approvals    []Approval `json:"approvals"`
	Rejection    *Rejection `json:"rejection,omitempty"`

	Materials []string `json:"materials,omitempty"`

	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	PublishedAt string `json:"publishedAt,omitempty"`
	PublishedBy string `json:"publishedBy,omitempty"`

	// --- emergency channel (`contracts/04` §7) ---
	//
	// Emergency publish is an **exception path**, so its evidence must be thicker than the normal path's: who triggered it, who co-authorized it,
	// when the makeup review is due, and whether it happened. Drop one item and the exception becomes the norm.
	Emergency         bool   `json:"emergency,omitempty"`
	EmergencyBy       string `json:"emergencyBy,omitempty"`
	EmergencySecondBy string `json:"emergencySecondBy,omitempty"`
	EmergencyAt       string `json:"emergencyAt,omitempty"`
	EmergencyReason   string `json:"emergencyReason,omitempty"`
	// MakeupReviewDueAt is the makeup review deadline (24h after the emergency publish, `contracts/04` §7)
	MakeupReviewDueAt string         `json:"makeupReviewDueAt,omitempty"`
	MakeupReviews     []MakeupReview `json:"makeupReviews,omitempty"`
}

// MakeupReview is a post-hoc review record. `Late` is recorded separately: an overdue makeup review and an on-time one carry different compliance meaning.
type MakeupReview struct {
	Actor     string `json:"actor"`
	ActorType string `json:"actorType"`
	Role      string `json:"role"`
	At        string `json:"at"`
	Late      bool   `json:"late"`
	Comment   string `json:"comment,omitempty"`
}

// MakeupReviewMissing reports whether an emergency publish has not been makeup-reviewed yet.
func (c Change) MakeupReviewMissing() bool {
	return c.Emergency && len(c.MakeupReviews) == 0
}

// Remaining returns the approval slots nobody has taken yet.
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

// Gray is the canary configuration: at runtime, "which version takes how much traffic" (`contracts/06` §5).
type Gray struct {
	PackageID string `json:"packageId"`
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	// Percentage is meaningful only in percentage mode; shadow is always 0 (it does not affect online decisions).
	Percentage int      `json:"percentage"`
	GrayTags   []string `json:"grayTags,omitempty"`
	// PeakPercentage is the historic peak rollout: needed to enforce "L4 must not jump straight to 100%".
	PeakPercentage int        `json:"peakPercentage"`
	UpdatedAt      string     `json:"updatedAt"`
	UpdatedBy      string     `json:"updatedBy"`
	History        []GrayStep `json:"history,omitempty"`
}

// GrayStep is one rollout adjustment (the rollout itself is an auditable configuration change, `contracts/06` §5.2).
type GrayStep struct {
	At         string `json:"at"`
	By         string `json:"by"`
	Mode       string `json:"mode"`
	Percentage int    `json:"percentage"`
	Reason     string `json:"reason,omitempty"`
}

// AuditEvent is an audit event (`contracts/04` §9). B9 records only "governance actions"; decision logs belong to B10.
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

// Audit action names.
const (
	ActionChangeSubmitted   = "CHANGE_SUBMITTED"
	ActionChangeApproved    = "CHANGE_APPROVED"
	ActionChangeRejected    = "CHANGE_REJECTED"
	ActionPackagePublished  = "PACKAGE_PUBLISHED"
	ActionPackageRolledBack = "PACKAGE_ROLLED_BACK"
	ActionGrayUpdated       = "GRAY_UPDATED"
	// Emergency channel (`contracts/04` §7): emergency publish and post-hoc makeup review each get their own event,
	// so questions like "how many emergency publishes this month, and are any of them left unreviewed" can be answered in one query.
	ActionChangeEmergency      = "CHANGE_EMERGENCY_PUBLISHED"
	ActionChangeMakeupReviewed = "CHANGE_MAKEUP_REVIEWED"
	// B10: raw log access and replay must also be audited (who looked at whose raw snapshot, and when).
	ActionLogRawAccessed = "LOG_RAW_ACCESSED"
	ActionReplayExecuted = "REPLAY_EXECUTED"
	// Key rotation and rewrap: rotation changes the premise of "who can read historic raw snapshots", so it must be audited.
	ActionLogKeyRotated = "LOG_KEY_ROTATED"
	ActionLogKeyRewrapped = "LOG_KEY_REWRAPPED"
)

// Doc is the governance repository document (a single JSON: change requests + canary configuration + audit events).
type Doc struct {
	Changes []Change        `json:"changes"`
	Grays   map[string]Gray `json:"grays"` // key = packageId
	Audits  []AuditEvent    `json:"audits"`
	// Seq is the per-day serial number: CHG-YYYYMMDD-0001.
	Seq map[string]int `json:"seq"`
}

// Error is a governance-side domain error. The HTTP mapping lives in httpapi; this package only states "which rule was violated".
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// NotFoundError means the change request / canary configuration does not exist.
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
