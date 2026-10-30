package gov

import "time"

// Approval SLA (`contracts/04` §8):
//
//	L1/L2 → target 1 business day, escalate to the approver's manager after 48h
//	L3    → target 4 hours, escalate to the owner after 24h
//	L4    → target 2 hours, escalate to the owner/compliance after 8h
//
// "1 business day" is approximated as 24 hours here: a real working calendar (holidays, cross-timezone) belongs to P1.
// It is stated out loud because **the approximation shifts the escalation moment** — better written as a comment than buried as an implicit constant.
const (
	slaTargetL12    = 24
	slaEscalateL12  = 48
	slaTargetL3     = 4
	slaEscalateL3   = 24
	slaTargetL4     = 2
	slaEscalateL4   = 8
	slaEscalateToUP = "approver-manager"
)

// SLA is a point-in-time SLA snapshot of one change request (for the frontend approval workbench and the overdue patrol).
type SLA struct {
	RiskLevel string `json:"riskLevel"`
	// Applicable: only the two "still waiting" states, submitted / approved, are subject to an SLA at all
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

// slaPolicy maps a level → (target hours, escalation hours, escalation target).
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

// SLA computes the current SLA state. `now` is supplied by the caller (consistent with Gov's clock injection, which keeps it testable).
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
		// Only two states are "still waiting": awaiting approval, and approved but not yet published. Terminal states (rejected/published) are no longer timed.
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
