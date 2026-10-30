package gov

import "time"

// 审批时效（`contracts/04` §8）：
//
//	L1/L2 → 目标 1 个工作日，超 48h 升级到审批人上级
//	L3    → 目标 4 小时，超 24h 升级到负责人
//	L4    → 目标 2 小时，超 8h 升级到负责人/合规
//
// 这里把「1 个工作日」近似为 24 小时：真正的工作日历（节假日、跨时区）属 P1。
// 之所以明说，是因为**近似值会影响升级时点**——把它写成注释比埋一个隐式常量好。
const (
	slaTargetL12    = 24
	slaEscalateL12  = 48
	slaTargetL3     = 4
	slaEscalateL3   = 24
	slaTargetL4     = 2
	slaEscalateL4   = 8
	slaEscalateToUP = "approver-manager"
)

// SLA 一张变更单的时效快照（给前端审批工作台与超时巡检用）。
type SLA struct {
	RiskLevel string `json:"riskLevel"`
	// Applicable 只有 submitted / approved 这两种「还在等」的状态才谈时效
	Applicable         bool    `json:"applicable"`
	TargetHours        int     `json:"targetHours"`
	EscalateAfterHours int     `json:"escalateAfterHours"`
	DeadlineAt         string  `json:"deadlineAt,omitempty"`
	EscalateAt         string  `json:"escalateAt,omitempty"`
	ElapsedHours       float64 `json:"elapsedHours"`
	Overdue            bool    `json:"overdue"`
	Escalated          bool    `json:"escalated"`
	EscalateTo         string  `json:"escalateTo,omitempty"`
}

// slaPolicy 等级 → (目标小时, 升级小时, 升级对象)。
func slaPolicy(level string) (int, int, string) {
	switch level {
	case LevelL1, LevelL2:
		return slaTargetL12, slaEscalateL12, "审批人上级"
	case LevelL3:
		return slaTargetL3, slaEscalateL3, "负责人"
	case LevelL4:
		return slaTargetL4, slaEscalateL4, "负责人/合规"
	}
	return 0, 0, ""
}

// SLA 计算当前时效状态。`now` 由调用方传入（与 Gov 的时钟注入保持一致，便于测试）。
func (c Change) SLA(now time.Time) SLA {
	target, escalate, to := slaPolicy(c.RiskLevel)
	created, err := time.Parse(time.RFC3339, c.CreatedAt)
	if err != nil || target == 0 {
		return SLA{RiskLevel: c.RiskLevel}
	}

	out := SLA{
		RiskLevel:          c.RiskLevel,
		TargetHours:        target,
		EscalateAfterHours: escalate,
		EscalateTo:         to,
		// 「还在等」只有两种：待审批、已批准待发布。终态（否决/已发布）不再计时。
		Applicable: c.Status == ChangeSubmitted || c.Status == ChangeApproved,
	}
	out.DeadlineAt = created.Add(time.Duration(target) * time.Hour).UTC().Format(time.RFC3339)
	out.EscalateAt = created.Add(time.Duration(escalate) * time.Hour).UTC().Format(time.RFC3339)

	elapsed := now.Sub(created)
	if elapsed < 0 {
		elapsed = 0
	}
	out.ElapsedHours = float64(int(elapsed.Hours()*10)) / 10
	out.Overdue = out.Applicable && elapsed > time.Duration(target)*time.Hour
	out.Escalated = out.Applicable && elapsed > time.Duration(escalate)*time.Hour
	return out
}
