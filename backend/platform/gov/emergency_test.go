package gov

import (
	"errors"
	"testing"
	"time"
)

// Approval SLA (contracts/04 §8) and the emergency channel (contracts/04 §7).

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	if err != nil {
		return "NON_GOV_ERROR: " + err.Error()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Approval SLA
// ---------------------------------------------------------------------------

func TestSLAPolicyPerLevel(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL1)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL1,
		Reason: "新增渠道", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	sla := c.SLA(e.now)
	if !sla.Applicable || sla.TargetHours != slaTargetL12 || sla.EscalateAfterHours != slaEscalateL12 {
		t.Fatalf("L1 时效应为 %d/%d 且生效: %+v", slaTargetL12, slaEscalateL12, sla)
	}
	if sla.Overdue || sla.Escalated {
		t.Fatalf("刚提交不应超时: %+v", sla)
	}
	if sla.DeadlineAt == "" || sla.EscalateAt == "" {
		t.Fatalf("应给出截止与升级时点: %+v", sla)
	}

	// Past the target but not yet past the escalation line
	mid := c.SLA(e.now.Add(time.Duration(slaTargetL12+1) * time.Hour))
	if !mid.Overdue || mid.Escalated {
		t.Fatalf("超目标 1h 应 Overdue 未 Escalated: %+v", mid)
	}
	// Past the escalation line
	late := c.SLA(e.now.Add(time.Duration(slaEscalateL12+1) * time.Hour))
	if !late.Overdue || !late.Escalated || late.EscalateTo == "" {
		t.Fatalf("超升级线应 Escalated 且给出升级对象: %+v", late)
	}
}

func TestSLAStopsCountingOnTerminalState(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL3)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL3,
		Reason: "调阈值", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	e.approveAll(t, c.ChangeID)
	e.publish(t, c.ChangeID, "v1")

	out, _ := e.svc.Get(c.ChangeID)
	sla := out.SLA(e.now.Add(100 * time.Hour))
	if sla.Applicable || sla.Overdue || sla.Escalated {
		t.Fatalf("终态不应再计时效: %+v", sla)
	}
}

func TestSLAHasZeroPolicyForUnknownLevel(t *testing.T) {
	c := Change{RiskLevel: "L9", CreatedAt: "2026-10-30T09:00:00Z"}
	if sla := c.SLA(time.Now()); sla.TargetHours != 0 || sla.Applicable {
		t.Fatalf("未知等级应给空策略而不是瞎猜: %+v", sla)
	}
}

// ---------------------------------------------------------------------------
// Emergency channel
// ---------------------------------------------------------------------------

func (e *env) l3Change(t *testing.T) Change {
	t.Helper()
	e.draft(t, "v1", LevelL3)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL3,
		Reason: "线上事故修复", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return c
}

// L3 may use the emergency channel: it skips waiting for approval, but must book the "makeup review within 24h" debt.
func TestEmergencyPublishL3RecordsMakeupReviewDebt(t *testing.T) {
	e := newEnv(t)
	c := e.l3Change(t)

	out, err := e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer,
		Reason: "线上提现被误拒，立即修复",
	})
	if err != nil {
		t.Fatalf("emergency publish: %v", err)
	}
	if !out.Emergency || out.Status != ChangePublished {
		t.Fatalf("应为紧急发布且已发布: %+v", out)
	}
	if out.MakeupReviewDueAt == "" || !out.MakeupReviewMissing() {
		t.Fatalf("必须记下补审截止且处于待补审: %+v", out)
	}
	// the version really is published
	active, ok := e.pkg.Active("pkg-withdraw")
	if !ok || active.PackageVersion != "v1" {
		t.Fatalf("规则包应已生效: %+v", active)
	}
	// the patrol can see this debt
	if got := len(e.svc.PendingMakeupReviews()); got != 1 {
		t.Fatalf("待补审应有 1 条，实际 %d", got)
	}
}

func TestEmergencyL4NeedsSecondActor(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL4)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL4,
		Reason: "合规口径变更", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	_, err = e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
	})
	if code := codeOf(t, err); code != CodeEmergencySecondRequired {
		t.Fatalf("L4 无共同授权应被拒 %s，实际 %s", CodeEmergencySecondRequired, code)
	}

	_, err = e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
		SecondActor: "release.R", SecondActorType: SubmitterEngineer,
	})
	if code := codeOf(t, err); code != CodeEmergencySecondInvalid {
		t.Fatalf("共同授权人不能是发布人本人，实际 %s", code)
	}

	_, err = e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
		SecondActor: "agent:helper", SecondActorType: SubmitterAgent,
	})
	if code := codeOf(t, err); code != CodeEmergencySecondInvalid {
		t.Fatalf("agent 不能共同授权，实际 %q", code)
	}

	out, err := e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
		SecondActor: "owner.U", SecondActorType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("双人授权应放行: %v", err)
	}
	if out.EmergencySecondBy != "owner.U" {
		t.Fatalf("应记录共同授权人: %+v", out)
	}
}

func TestEmergencyRejectsAgentAndEmptyReason(t *testing.T) {
	e := newEnv(t)
	c := e.l3Change(t)

	_, err := e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "agent:x", ActorType: SubmitterAgent, Reason: "我想发",
	})
	if code := codeOf(t, err); code != CodeAgentCannotPublish {
		t.Fatalf("agent 紧急发布应被拒，实际 %s", code)
	}

	_, err = e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "  ",
	})
	if code := codeOf(t, err); code != CodeInvalidArgument {
		t.Fatalf("无原因应被拒，实际 %s", code)
	}
}

func TestMakeupReviewRejectsSelfAndDuplicate(t *testing.T) {
	e := newEnv(t)
	c := e.l3Change(t)
	if _, err := e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
	}); err != nil {
		t.Fatalf("emergency: %v", err)
	}

	// publishing yourself and reviewing yourself = no review
	_, err := e.svc.MakeupReview(c.ChangeID, "release.R", SubmitterEngineer, RoleEngineer, "我已复核")
	if code := codeOf(t, err); code != CodeMakeupReviewSelf {
		t.Fatalf("发布人自审应被拒，实际 %s", code)
	}

	out, err := e.svc.MakeupReview(c.ChangeID, "engineer.Z", SubmitterEngineer, RoleEngineer, "复核通过")
	if err != nil {
		t.Fatalf("补审应成功: %v", err)
	}
	if len(out.MakeupReviews) != 1 || out.MakeupReviews[0].Late {
		t.Fatalf("应记录按时补审: %+v", out.MakeupReviews)
	}
	if len(e.svc.PendingMakeupReviews()) != 0 {
		t.Fatalf("补审后不应再出现在待补审列表")
	}

	_, err = e.svc.MakeupReview(c.ChangeID, "risk.W", SubmitterEngineer, RoleRisk, "再来一次")
	if code := codeOf(t, err); code != CodeMakeupReviewDuplicate {
		t.Fatalf("重复补审应被拒，实际 %s", code)
	}
}

// An overdue makeup review is not blocked, but must be marked Late — refusing would leave "a change that already shipped" permanently unreviewed.
func TestMakeupReviewMarksLateButStillSucceeds(t *testing.T) {
	e := newEnv(t)
	c := e.l3Change(t)
	if _, err := e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
	}); err != nil {
		t.Fatalf("emergency: %v", err)
	}

	e.svc.WithClock(func() time.Time { return e.now.Add(makeupReviewWindow + time.Hour) })
	out, err := e.svc.MakeupReview(c.ChangeID, "engineer.Z", SubmitterEngineer, RoleEngineer, "事后复核")
	if err != nil {
		t.Fatalf("超期补审应允许: %v", err)
	}
	if !out.MakeupReviews[0].Late {
		t.Fatalf("超期补审应标 Late: %+v", out.MakeupReviews[0])
	}
}

// A normal (non-emergency) change request does not allow a makeup review: this avoids "publish first, then add a fake makeup review".
func TestMakeupReviewOnlyForEmergencyChanges(t *testing.T) {
	e := newEnv(t)
	c := e.l3Change(t)
	_, err := e.svc.MakeupReview(c.ChangeID, "engineer.Z", SubmitterEngineer, RoleEngineer, "复核")
	if code := codeOf(t, err); code != CodeMakeupReviewRequired {
		t.Fatalf("非紧急单补审应被拒，实际 %s", code)
	}
}

// The emergency channel is audited just the same: one event for the emergency publish and one for the regular publish, so the publish history has no hole.
func TestEmergencyPublishIsAudited(t *testing.T) {
	e := newEnv(t)
	c := e.l3Change(t)
	if _, err := e.svc.EmergencyPublish(EmergencyInput{
		ChangeID: c.ChangeID, Actor: "release.R", ActorType: SubmitterEngineer, Reason: "紧急",
	}); err != nil {
		t.Fatalf("emergency: %v", err)
	}

	var emergency, published int
	for _, ev := range e.svc.Audits(c.ChangeID, "") {
		switch ev.Action {
		case ActionChangeEmergency:
			emergency++
		case ActionPackagePublished:
			published++
		}
	}
	if emergency != 1 || published != 1 {
		t.Fatalf("应各留 1 条事件，实际 emergency=%d published=%d", emergency, published)
	}
}
