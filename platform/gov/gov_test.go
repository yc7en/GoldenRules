package gov

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
	"goldenrules/platform/packages"
	"goldenrules/platform/persist"
)

func testCatalog() contracts.FactCatalog {
	return contracts.FactCatalog{
		CatalogVersion: "catalog-v2026.10.1",
		Facts: []contracts.FactSchema{
			{Fact: "request.amount", Type: "decimal", Unit: "CNY", Required: true, Semantics: "提款金额"},
		},
	}
}

func testDef() dsl.Definition {
	return dsl.Definition{
		RulesetID:       "rs-amount-limit",
		Scene:           "withdraw-risk-check",
		Version:         "rs-1",
		HitStrategy:     "ALL",
		DefaultDecision: "REJECT",
		Rules: []contracts.Rule{{
			RuleID:   "R1",
			Priority: 100,
			When:     json.RawMessage(`"request.amount > 1000"`),
			Then:     contracts.Then{Decision: "REJECT", ReasonCode: "AMOUNT_LIMIT_EXCEEDED"},
		}},
	}
}

// env 一套打通的治理环境：事实目录 + 规则集 + 规则包 + 治理服务。
type env struct {
	svc *Service
	pkg *packages.Service
	cat contracts.FactCatalog
	def dsl.Definition
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	clock := func() time.Time { return time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC) }

	pkStore, err := persist.Open(filepath.Join(dir, "packages.json"), func() packages.Repo {
		return packages.Repo{Active: map[string]string{}}
	})
	if err != nil {
		t.Fatalf("open packages: %v", err)
	}
	pkgSvc := packages.NewService(pkStore).WithClock(clock)

	docStore, err := persist.Open(filepath.Join(dir, "gov.json"), func() Doc {
		return Doc{Grays: map[string]Gray{}, Seq: map[string]int{}}
	})
	if err != nil {
		t.Fatalf("open gov: %v", err)
	}
	svc := NewService(docStore, pkgSvc).WithClock(clock)

	return &env{svc: svc, pkg: pkgSvc, cat: testCatalog(), def: testDef(), now: clock()}
}

// draft 造一个草稿版本。
func (e *env) draft(t *testing.T, version, risk string) packages.Version {
	t.Helper()
	v, _, err := e.pkg.CreateDraft(packages.BuildInput{
		PackageID: "pkg-withdraw",
		Version:   version,
		Rulesets:  []dsl.Definition{e.def},
		Catalog:   e.cat,
		Actor:     "engineer.A",
		Changelog: "测试",
		RiskLevel: risk,
	})
	if err != nil {
		t.Fatalf("create draft %s: %v", version, err)
	}
	return v
}

// publish 走完整治理链：过 PermitPublish → 真发布 → 变更单收尾 + 审计。
func (e *env) publish(t *testing.T, changeID, version string) {
	t.Helper()
	if _, err := e.svc.PermitPublish(changeID, "pkg-withdraw", version, "release.R", SubmitterEngineer); err != nil {
		t.Fatalf("permit publish %s: %v", version, err)
	}
	prev, _ := e.pkg.Active("pkg-withdraw")
	v, err := e.pkg.Publish("pkg-withdraw", version, "release.R", "", "")
	if err != nil {
		t.Fatalf("publish %s: %v", version, err)
	}
	e.svc.MarkPublished(changeID, v, "release.R", SubmitterEngineer, prev.PackageVersion, "", "")
}

// approveAll 按矩阵把一张单子批满（除 submitter 之外的合适人选）。
func (e *env) approveAll(t *testing.T, changeID string) Change {
	t.Helper()
	c, ok := e.svc.Get(changeID)
	if !ok {
		t.Fatalf("变更单不存在: %s", changeID)
	}
	actors := map[string]string{
		RoleEngineer:   "engineer.Z",
		RolePeer:       "engineer.Y",
		RoleBusiness:   "business.X",
		RoleRisk:       "risk.W",
		RoleCompliance: "compliance.V",
		RoleOwner:      "owner.U",
	}
	for _, s := range c.Remaining() {
		role := s.Roles[0]
		actor := actors[role]
		if actor == c.Submitter {
			actor = "backup." + actor
		}
		if _, err := e.svc.Approve(changeID, actor, SubmitterEngineer, role, "ok"); err != nil {
			t.Fatalf("approve %s as %s/%s: %v", changeID, actor, role, err)
		}
	}
	out, _ := e.svc.Get(changeID)
	return out
}

// ---------------------------------------------------------------------------

// 硬规则 A-8：agent 不得直接提交 L4。
func TestAgentCannotSubmitL4(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL4)

	_, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1",
		RiskLevel: LevelL4, Reason: "改制裁名单", Submitter: "agent:rule-writer", SubmitterType: SubmitterAgent,
	})
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != CodeMatrixRejected {
		t.Fatalf("期望 %s，实际 %v", CodeMatrixRejected, err)
	}
	if len(e.svc.List("", "")) != 0 {
		t.Fatalf("被矩阵拒绝的提交不应落库")
	}
}

// 矩阵按等级要人：L3 工程师提交 → 需 2 人，第一位之后仍未通过。
func TestEngineerL3NeedsTwoApprovals(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL3)

	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL3,
		Reason: "调整限额", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(c.Slots) != 2 || c.Status != ChangeSubmitted {
		t.Fatalf("L3 应有 2 个审批位: %+v", c)
	}

	c, err = e.svc.Approve(c.ChangeID, "engineer.Z", SubmitterEngineer, RoleEngineer, "看过了")
	if err != nil {
		t.Fatalf("第一次审批: %v", err)
	}
	if c.Status != ChangeSubmitted || len(c.Remaining()) != 1 {
		t.Fatalf("一个人批完不应通过: %s 剩 %d", c.Status, len(c.Remaining()))
	}
	if v, _ := e.pkg.Get("pkg-withdraw", "v1"); v.Status != packages.StatusDraft {
		t.Fatalf("未满票时版本应仍是 draft，实际 %s", v.Status)
	}

	c, err = e.svc.Approve(c.ChangeID, "risk.W", SubmitterEngineer, RoleRisk, "风控同意")
	if err != nil {
		t.Fatalf("第二次审批: %v", err)
	}
	if c.Status != ChangeApproved {
		t.Fatalf("满票应 approved，实际 %s", c.Status)
	}
	if v, _ := e.pkg.Get("pkg-withdraw", "v1"); v.Status != packages.StatusApproved {
		t.Fatalf("满票后版本应推进到 approved，实际 %s", v.Status)
	}
}

// 「自审」只在矩阵写了自审的格子里成立。
func TestSelfApprovalOnlyWhenMatrixAllows(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL3)

	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL3,
		Reason: "调整限额", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	_, err = e.svc.Approve(c.ChangeID, "engineer.A", SubmitterEngineer, RoleEngineer, "自己批自己")
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != CodeSelfApproval {
		t.Fatalf("L3 自审应被拒，实际 %v", err)
	}

	// L1 允许自审
	e2 := newEnv(t)
	e2.draft(t, "v1", LevelL1)
	c2, err := e2.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL1,
		Reason: "营销文案", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit L1: %v", err)
	}
	c2, err = e2.svc.Approve(c2.ChangeID, "engineer.A", SubmitterEngineer, RoleEngineer, "自审通过")
	if err != nil {
		t.Fatalf("L1 自审应允许: %v", err)
	}
	if len(c2.Approvals) != 1 || !c2.Approvals[0].Self {
		t.Fatalf("自审应标记 self=true: %+v", c2.Approvals)
	}
}

// 职责分离：提交者、审批人、agent 都不能发布。
func TestPublishSeparationOfDuties(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL3)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL3,
		Reason: "调整限额", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	e.approveAll(t, c.ChangeID)

	cases := []struct {
		actor, actorType, want string
	}{
		{"engineer.A", SubmitterEngineer, CodeSoDSubmitPublish},
		{"engineer.Z", SubmitterEngineer, CodeSoDApprovePublish},
		{"release.R", SubmitterAgent, CodeAgentCannotPublish},
	}
	for _, tc := range cases {
		_, err := e.svc.PermitPublish(c.ChangeID, "pkg-withdraw", "v1", tc.actor, tc.actorType)
		var ge *Error
		if !errors.As(err, &ge) || ge.Code != tc.want {
			t.Fatalf("%s 发布应被 %s 拒绝，实际 %v", tc.actor, tc.want, err)
		}
	}

	// 第三方发布人可以
	if _, err := e.svc.PermitPublish(c.ChangeID, "pkg-withdraw", "v1", "release.R", SubmitterEngineer); err != nil {
		t.Fatalf("发布人应被允许: %v", err)
	}
	// 目标不一致
	_, err = e.svc.PermitPublish(c.ChangeID, "pkg-withdraw", "v9", "release.R", SubmitterEngineer)
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != CodePackageMismatch {
		t.Fatalf("版本不符应 %s，实际 %v", CodePackageMismatch, err)
	}
	// 没挂变更单
	_, err = e.svc.PermitPublish("", "pkg-withdraw", "v1", "release.R", SubmitterEngineer)
	if !errors.As(err, &ge) || ge.Code != CodeChangeRequired {
		t.Fatalf("缺变更单应 %s，实际 %v", CodeChangeRequired, err)
	}
}

// 单据的风险等级不能低于包内声明（防降级绕过）。
func TestRiskLevelMustMatchPackage(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL4)

	_, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL1,
		Reason: "偷偷降级", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != CodeInvalidArgument {
		t.Fatalf("等级不符应被拒，实际 %v", err)
	}
}

// 否决是终态，且必须写理由。
func TestRejectIsTerminal(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL2)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL2,
		Reason: "调阈值", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := e.svc.Reject(c.ChangeID, "engineer.Z", SubmitterEngineer, RolePeer, ""); err == nil {
		t.Fatalf("否决不写理由应被拒")
	}
	c, err = e.svc.Reject(c.ChangeID, "engineer.Z", SubmitterEngineer, RolePeer, "阈值依据不足")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if c.Status != ChangeRejected || c.Rejection == nil {
		t.Fatalf("否决后状态异常: %+v", c)
	}
	if _, err := e.svc.Approve(c.ChangeID, "engineer.Y", SubmitterEngineer, RolePeer, "改主意了"); err == nil {
		t.Fatalf("已否决的单子不能再批")
	}
	if v, _ := e.pkg.Get("pkg-withdraw", "v1"); v.Status != packages.StatusDraft {
		t.Fatalf("否决不应推进版本状态，实际 %s", v.Status)
	}
}

// L4 不得一步到 100%，必须先有中间档（`contracts/04` §6）。
func TestGrayStagedForL4(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL4)
	c, err := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL4,
		Reason: "制裁名单", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	e.approveAll(t, c.ChangeID)
	e.publish(t, c.ChangeID, "v1")

	// 直接 100%
	_, _, err = e.svc.SetGray(GrayInput{
		PackageID: "pkg-withdraw", Version: "v1", Mode: GrayPercentage, Percentage: 100,
		Actor: "release.R", ActorType: SubmitterEngineer, Reason: "冲",
	})
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != CodeGrayStepTooLarge {
		t.Fatalf("L4 一步到 100%% 应被拒，实际 %v", err)
	}

	// 首档也不能太大
	_, _, err = e.svc.SetGray(GrayInput{
		PackageID: "pkg-withdraw", Version: "v1", Mode: GrayPercentage, Percentage: 30,
		Actor: "release.R", ActorType: SubmitterEngineer,
	})
	if !errors.As(err, &ge) || ge.Code != CodeGrayStepTooLarge {
		t.Fatalf("L4 首档 30%% 应被拒，实际 %v", err)
	}

	// 5% → 50% → 100%
	for _, p := range []int{5, 50, 100} {
		if _, _, err := e.svc.SetGray(GrayInput{
			PackageID: "pkg-withdraw", Version: "v1", Mode: GrayPercentage, Percentage: p,
			Actor: "release.R", ActorType: SubmitterEngineer, Reason: "分批放量",
		}); err != nil {
			t.Fatalf("放量到 %d%% 应允许: %v", p, err)
		}
	}
	g, ok := e.svc.Gray("pkg-withdraw")
	if !ok || g.Percentage != 100 || g.PeakPercentage != 100 || len(g.History) != 3 {
		t.Fatalf("灰度终态异常: %+v", g)
	}
}

// 灰度三形态的形态约束。
func TestGrayModeConstraints(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL1)
	c, _ := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL1,
		Reason: "营销参数", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	e.approveAll(t, c.ChangeID)
	e.publish(t, c.ChangeID, "v1")

	bad := []GrayInput{
		{PackageID: "pkg-withdraw", Version: "v1", Mode: "canary", Percentage: 10},
		{PackageID: "pkg-withdraw", Version: "v1", Mode: GrayShadow, Percentage: 10},
		{PackageID: "pkg-withdraw", Version: "v1", Mode: GrayPercentage, Percentage: 0},
		{PackageID: "pkg-withdraw", Version: "v1", Mode: GrayConditional},
		{PackageID: "pkg-withdraw", Version: "v1", Mode: GrayPercentage, Percentage: 120},
	}
	for _, in := range bad {
		in.Actor, in.ActorType = "release.R", SubmitterEngineer
		if _, _, err := e.svc.SetGray(in); err == nil {
			t.Fatalf("非法灰度应被拒: %+v", in)
		}
	}

	if _, _, err := e.svc.SetGray(GrayInput{
		PackageID: "pkg-withdraw", Version: "v1", Mode: GrayConditional, GrayTags: []string{"gray-1"},
		Actor: "release.R", ActorType: SubmitterEngineer,
	}); err != nil {
		t.Fatalf("条件灰度应允许: %v", err)
	}
}

// 回滚留痕：审计里能还原「谁、何时、从哪个版本切到哪个版本、为什么」。
func TestRollbackIsAudited(t *testing.T) {
	e := newEnv(t)
	e.draft(t, "v1", LevelL1)
	c, _ := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v1", RiskLevel: LevelL1,
		Reason: "一版", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	e.approveAll(t, c.ChangeID)
	e.publish(t, c.ChangeID, "v1")
	e.draft(t, "v2", LevelL1)
	c2, _ := e.svc.Submit(SubmitInput{
		PackageID: "pkg-withdraw", PackageVersion: "v2", RiskLevel: LevelL1,
		Reason: "二版", Submitter: "engineer.A", SubmitterType: SubmitterEngineer,
	})
	e.approveAll(t, c2.ChangeID)
	e.publish(t, c2.ChangeID, "v2")

	if _, err := e.svc.Rollback("pkg-withdraw", "v1", "release.R", SubmitterAgent, "agent 不能回滚"); err == nil {
		t.Fatalf("agent 回滚应被拒")
	}
	if _, err := e.svc.Rollback("pkg-withdraw", "v1", "release.R", SubmitterEngineer, ""); err == nil {
		t.Fatalf("回滚不写原因应被拒")
	}
	v, err := e.svc.Rollback("pkg-withdraw", "v1", "release.R", SubmitterEngineer, "线上误拒率升高")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if v.PackageVersion != "v1" || v.Status != packages.StatusPublished {
		t.Fatalf("回滚后应生效 v1: %+v", v)
	}
	if n := len(e.pkg.List("pkg-withdraw")); n != 2 {
		t.Fatalf("回滚不应产生新版本号，版本数 %d", n)
	}

	evs := e.svc.Audits("", "pkg-withdraw")
	var rollback *AuditEvent
	for i := range evs {
		if evs[i].Action == ActionPackageRolledBack {
			rollback = &evs[i]
		}
	}
	if rollback == nil {
		t.Fatalf("缺回滚审计事件: %+v", evs)
	}
	if rollback.From != "v2" || rollback.To != "v1" || rollback.Reason != "线上误拒率升高" || rollback.Actor != "release.R" {
		t.Fatalf("回滚审计字段不全: %+v", rollback)
	}
	// 发布也要留痕
	var publishes int
	for _, ev := range evs {
		if ev.Action == ActionPackagePublished {
			publishes++
		}
	}
	if publishes != 2 {
		t.Fatalf("应记录 2 次发布，实际 %d", publishes)
	}
}

// 矩阵导出给前端：12 格齐全，agent×L4 标记禁止。
func TestDescribeMatrix(t *testing.T) {
	cells := Describe()
	if len(cells) != 12 {
		t.Fatalf("矩阵应有 12 格，实际 %d", len(cells))
	}
	for _, c := range cells {
		if c["submitterType"] == SubmitterAgent && c["riskLevel"] == LevelL4 {
			if c["forbidden"] != true || c["requiredCount"] != 0 {
				t.Fatalf("agent×L4 应是禁止格: %+v", c)
			}
		}
	}
}
