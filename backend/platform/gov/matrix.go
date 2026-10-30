// Package gov implements B9: approval matrix, segregation of duties, canary and publish governance.
//
// Contract basis: `contracts/04` (risk grading / approval matrix / segregation of duties / audit retention),
// `contracts/06` §4–§6 (state machine / canary / rollback).
//
// The relationship with `packages` is **one-way**: gov calls packages (advancing the version state after approval, publishing, rolling back),
// and packages does not know gov exists. That way B8's publish capability can be used on its own (CI, drills),
// while questions like "who approved this" and "may this be published" are answered only inside gov.
package gov

import "strings"

// Submitter types (the rows of the `contracts/04` §3 matrix).
const (
	SubmitterEngineer = "engineer"
	SubmitterBusiness = "business"
	SubmitterAgent    = "agent"
)

// Approval roles (`contracts/04` §11.C).
const (
	RoleEngineer   = "engineer"   // engineer (may self-review)
	RolePeer       = "peer"       // peer engineer (review)
	RoleBusiness   = "business"   // business
	RoleRisk       = "risk"       // risk
	RoleCompliance = "compliance" // compliance
	RoleOwner      = "owner"      // owner
)

// Risk levels (`contracts/04` §2).
const (
	LevelL1 = "L1"
	LevelL2 = "L2"
	LevelL3 = "L3"
	LevelL4 = "L4"
)

// Slot is one approval slot waiting to be filled.
//
// Roles is **the set of acceptable roles**, not a single role: when the matrix says "two people (including 1 business/risk)",
// who takes that slot is decided by business reality, and the platform should not pick people on the customer's behalf.
type Slot struct {
	Roles []string `json:"roles"`
	Label string   `json:"label"`
}

// Accepts reports whether this approval slot accepts a given role.
func (s Slot) Accepts(role string) bool {
	for _, r := range s.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Cell is one cell of the matrix: a submitter type × a risk level.
type Cell struct {
	Slots []Slot
	// SelfApproval says whether the submitter himself may occupy an approval slot.
	// `contracts/04` §3's "self-approval" and §4's "the approver must not be the submitter" converge here:
	// only cells that explicitly say "self-approval" allow it; everything else is refused.
	SelfApproval bool
	// Forbidden is a hard rule: an agent must not submit L4 directly (A-8).
	Forbidden bool
	Reason    string
}

// matrix is a literal transcription of the `contracts/04` §3 table. Changing the matrix requires changing the contract in step.
var matrix = map[string]map[string]Cell{
	SubmitterEngineer: {
		LevelL1: {Slots: []Slot{{Roles: []string{RoleEngineer}, Label: "工程师自审"}}, SelfApproval: true},
		LevelL2: {Slots: []Slot{
			{Roles: []string{RoleEngineer}, Label: "工程师自审"},
			{Roles: []string{RolePeer}, Label: "同行 review"},
		}, SelfApproval: true},
		LevelL3: {Slots: []Slot{
			{Roles: []string{RoleEngineer}, Label: "工程师"},
			{Roles: []string{RoleBusiness, RoleRisk}, Label: "业务或风控"},
		}},
		LevelL4: {Slots: []Slot{
			{Roles: []string{RoleEngineer}, Label: "工程师"},
			{Roles: []string{RoleBusiness, RoleRisk}, Label: "业务或风控"},
			{Roles: []string{RoleCompliance, RoleOwner}, Label: "合规或负责人"},
		}},
	},
	SubmitterBusiness: {
		LevelL1: {Slots: []Slot{{Roles: []string{RoleBusiness}, Label: "业务自审"}}, SelfApproval: true},
		LevelL2: {Slots: []Slot{{Roles: []string{RoleEngineer}, Label: "工程师复核"}}},
		LevelL3: {Slots: []Slot{
			{Roles: []string{RoleEngineer}, Label: "工程师"},
			{Roles: []string{RoleRisk}, Label: "风控"},
		}},
		LevelL4: {Slots: []Slot{
			{Roles: []string{RoleEngineer}, Label: "工程师"},
			{Roles: []string{RoleRisk}, Label: "风控"},
			{Roles: []string{RoleOwner}, Label: "负责人"},
		}},
	},
	SubmitterAgent: {
		LevelL1: {Slots: []Slot{{Roles: []string{RoleEngineer}, Label: "工程师复核"}}},
		LevelL2: {Slots: []Slot{{Roles: []string{RoleEngineer}, Label: "工程师复核"}}},
		LevelL3: {Slots: []Slot{
			{Roles: []string{RoleEngineer}, Label: "工程师"},
			{Roles: []string{RoleRisk}, Label: "风控"},
		}},
		LevelL4: {Forbidden: true, Reason: "agent 不得直接提交 L4 变更，只能产出建议供人工录入（contracts/04 §3 硬规则 A-8）"},
	},
}

// Lookup fetches one cell of the matrix.
func Lookup(submitter, level string) (Cell, bool) {
	byLevel, ok := matrix[submitter]
	if !ok {
		return Cell{}, false
	}
	c, ok := byLevel[level]
	return c, ok
}

// Describe exports the whole matrix (for the frontend approval workbench to render and for audit reconciliation).
func Describe() []map[string]any {
	out := make([]map[string]any, 0, 12)
	for _, submitter := range []string{SubmitterEngineer, SubmitterBusiness, SubmitterAgent} {
		for _, level := range []string{LevelL1, LevelL2, LevelL3, LevelL4} {
			c, _ := Lookup(submitter, level)
			roles := make([][]string, 0, len(c.Slots))
			labels := make([]string, 0, len(c.Slots))
			for _, s := range c.Slots {
				roles = append(roles, s.Roles)
				labels = append(labels, s.Label)
			}
			out = append(out, map[string]any{
				"submitterType":  submitter,
				"riskLevel":      level,
				"requiredCount":  len(c.Slots),
				"slotRoles":      roles,
				"slotLabels":     labels,
				"selfApproval":   c.SelfApproval,
				"forbidden":      c.Forbidden,
				"forbiddenCause": c.Reason,
			})
		}
	}
	return out
}

// validSubmitter / validLevel keep a mistyped enum from reaching the submit entry point and turning into a matrix lookup miss.
func validSubmitter(s string) bool {
	switch s {
	case SubmitterEngineer, SubmitterBusiness, SubmitterAgent:
		return true
	}
	return false
}

func validLevel(l string) bool {
	switch l {
	case LevelL1, LevelL2, LevelL3, LevelL4:
		return true
	}
	return false
}

func validRole(r string) bool {
	switch r {
	case RoleEngineer, RolePeer, RoleBusiness, RoleRisk, RoleCompliance, RoleOwner:
		return true
	}
	return false
}

// NormalizeActorType collapses case/whitespace differences in the header, and treats anything unrecognized as least privilege (agent).
//
// This is **deliberate fail-closed**: before authentication is wired in at P1, callers self-report their identity via `X-Actor-Type`;
// defaulting to engineer when unrecognized would hand out "self-approval + publish" for free, so both the missing and the mistyped value land on agent
// (an agent can only submit drafts; it can neither approve nor publish).
func NormalizeActorType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "engineer", "工程师":
		return SubmitterEngineer
	case "business", "业务":
		return SubmitterBusiness
	}
	return SubmitterAgent
}
