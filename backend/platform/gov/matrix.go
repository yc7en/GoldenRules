// Package gov 实现 B9：审批矩阵、职责分离、灰度与发布治理。
//
// 契约依据：`contracts/04`（风险分级 / 审批矩阵 / 职责分离 / 审计留存）、
// `contracts/06` §4–§6（状态机 / 灰度 / 回滚）。
//
// 与 `packages` 的关系是**单向**的：gov 调 packages（审批通过后推进版本状态、发布、回滚），
// packages 不知道 gov 的存在。这样 B8 的发布能力可以单独使用（CI、演练），
// 而「谁批准」「能不能发」这类问题只在 gov 里回答。
package gov

import "strings"

// 提交者类型（`contracts/04` §3 矩阵的行）。
const (
	SubmitterEngineer = "engineer"
	SubmitterBusiness = "business"
	SubmitterAgent    = "agent"
)

// 审批角色（`contracts/04` §11.C）。
const (
	RoleEngineer   = "engineer"   // 工程师（可自审）
	RolePeer       = "peer"       // 同行工程师（review）
	RoleBusiness   = "business"   // 业务
	RoleRisk       = "risk"       // 风控
	RoleCompliance = "compliance" // 合规
	RoleOwner      = "owner"      // 负责人
)

// 风险等级（`contracts/04` §2）。
const (
	LevelL1 = "L1"
	LevelL2 = "L2"
	LevelL3 = "L3"
	LevelL4 = "L4"
)

// Slot 一个待填的审批位。
//
// Roles 是**可接受的角色集合**而不是单一角色：矩阵里写「双人（含 1 名业务/风控）」时，
// 这一位谁来做是业务现实决定的，不该由平台替客户挑人。
type Slot struct {
	Roles []string `json:"roles"`
	Label string   `json:"label"`
}

// Accepts 该审批位是否接受某角色。
func (s Slot) Accepts(role string) bool {
	for _, r := range s.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Cell 矩阵的一格：某提交者类型 × 某风险等级。
type Cell struct {
	Slots []Slot
	// SelfApproval 是否允许提交者本人占用审批位。
	// `contracts/04` §3 的「自审」与 §4 的「审批人不得是提交者本人」在此收口：
	// 只有矩阵明确写「自审」的格子才允许，其余一律拒绝。
	SelfApproval bool
	// Forbidden 硬规则：agent 不得直提 L4（A-8）。
	Forbidden bool
	Reason    string
}

// matrix 是 `contracts/04` §3 表格的直译。改矩阵必须同步改契约。
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

// Lookup 取矩阵的一格。
func Lookup(submitter, level string) (Cell, bool) {
	byLevel, ok := matrix[submitter]
	if !ok {
		return Cell{}, false
	}
	c, ok := byLevel[level]
	return c, ok
}

// Describe 导出一整张矩阵（给前端审批工作台渲染与审计对账用）。
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

// validSubmitter / validLevel 用于把「拼错的枚举」挡在提交入口，而不是变成矩阵查不到。
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

// NormalizeActorType 把请求头里的大小写/空白差异收敛，认不出时按最小权限（agent）处理。
//
// 这是**故意的 fail-closed**：P1 接入认证前，调用方靠 `X-Actor-Type` 自报身份；
// 认不出时若默认成工程师，等于凭空给了「自审 + 发布」权，所以缺省与拼错一律落到 agent
// （agent 只能提草稿，不能审批、不能发布）。
func NormalizeActorType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "engineer", "工程师":
		return SubmitterEngineer
	case "business", "业务":
		return SubmitterBusiness
	}
	return SubmitterAgent
}
