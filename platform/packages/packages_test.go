package packages

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
	"goldenrules/platform/persist"
)

func loadCatalog(t *testing.T) contracts.FactCatalog {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "fact-catalog.json"))
	if err != nil {
		t.Fatalf("读 fixtures 失败: %v", err)
	}
	var cat contracts.FactCatalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatalf("解析 fixtures 失败: %v", err)
	}
	return cat
}

func defAmount(cat contracts.FactCatalog) dsl.Definition {
	return dsl.Definition{
		RulesetID:       "rs-amount-limit",
		Scene:           "withdraw-risk-check",
		Version:         "rs-1",
		HitStrategy:     "ALL",
		DefaultDecision: "REJECT",
		Rules: []contracts.Rule{{
			RuleID:   "R2001",
			Priority: 100,
			When:     json.RawMessage(`{"op":"gt","args":[{"fact":"request.amount"},{"fact":"config.withdrawDailyLimit"}]}`),
			Then: contracts.Then{
				Decision:   "REJECT",
				ReasonCode: "AMOUNT_LIMIT_EXCEEDED",
				Assign: []contracts.Assign{
					{Var: "excessAmount", Expr: json.RawMessage(`{"op":"sub","args":[{"fact":"request.amount"},{"fact":"config.withdrawDailyLimit"}]}`)},
				},
			},
		}},
		OutputSchema: &contracts.OutputSchema{
			OutputSchemaID:      "withdraw-risk-output",
			OutputSchemaVersion: "v1",
			Fields: map[string]contracts.OutputField{
				"excessAmount": {Type: "decimal", Nullable: true},
			},
		},
		Tests: []contracts.TestCase{{
			TestID: "T001",
			Scene:  "withdraw-risk-check",
			Input:  map[string]json.RawMessage{"request.amount": json.RawMessage(`"1500"`)},
			Expect: contracts.Expectation{Decision: "REJECT", ReasonCode: "AMOUNT_LIMIT_EXCEEDED"},
			Author: "agent",
			Status: "pending_review",
		}},
	}
}

func newSvc(t *testing.T) *Service {
	t.Helper()
	st, err := persist.Open(filepath.Join(t.TempDir(), "packages.json"), func() Repo {
		return Repo{Active: map[string]string{}}
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return NewService(st).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})
}

func mustDraft(t *testing.T, svc *Service, cat contracts.FactCatalog, version string) Version {
	t.Helper()
	v, reports, err := svc.CreateDraft(BuildInput{
		PackageID: "pkg-withdraw-risk",
		Version:   version,
		Rulesets:  []dsl.Definition{defAmount(cat)},
		Catalog:   cat,
		Actor:     "engineer.A",
		Changelog: "加入额度校验",
		RiskLevel: "L3",
	})
	if err != nil {
		t.Fatalf("create draft: %v (%+v)", err, reports)
	}
	return v
}

// 验收 1：发布后产物不可变；二次发布生成新版本号；旧版本可回滚。
func TestPublishImmutableAndVersioned(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)

	v1 := mustDraft(t, svc, cat, "")
	if v1.PackageVersion != "v1" {
		t.Fatalf("首个版本号 = %q", v1.PackageVersion)
	}
	if v1.Status != StatusDraft {
		t.Fatalf("初始状态 = %s", v1.Status)
	}
	if v1.RuleCount != 1 || v1.UnconfirmedTC != 1 {
		t.Fatalf("统计不对: rules=%d pendingTC=%d", v1.RuleCount, v1.UnconfirmedTC)
	}

	// 未批准不能发布
	if _, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", ""); err == nil {
		t.Fatal("draft 不应能直接发布")
	}
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 重复发布幂等：仍是 v1，不产生新版本
	again, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", "")
	if err != nil {
		t.Fatalf("幂等发布应成功: %v", err)
	}
	if again.PackageVersion != "v1" {
		t.Fatalf("幂等发布改了版本号: %s", again.PackageVersion)
	}
	if n := len(svc.List("pkg-withdraw-risk")); n != 1 {
		t.Fatalf("版本数 = %d", n)
	}

	// 已发布版本不可再改：同版本号再建草稿必须被拒
	_, _, err = svc.CreateDraft(BuildInput{
		PackageID: "pkg-withdraw-risk", Version: "v1",
		Rulesets: []dsl.Definition{defAmount(cat)}, Catalog: cat, Actor: "engineer.A",
	})
	var se *StateError
	if !errors.As(err, &se) || se.Code != CodeVersionExists {
		t.Fatalf("期望 %s，得到 %v", CodeVersionExists, err)
	}

	// 第二次发布 → 新版本号 v2，v1 被取代退役
	v2 := mustDraft(t, svc, cat, "")
	if v2.PackageVersion != "v2" {
		t.Fatalf("第二个版本号 = %q", v2.PackageVersion)
	}
	if v2.Signature == v1.Signature {
		t.Fatal("内容相同但时间不同，签名不应相同")
	}
	if _, err := svc.Approve("pkg-withdraw-risk", "v2", "reviewer.B"); err != nil {
		t.Fatalf("approve v2: %v", err)
	}
	if _, err := svc.Publish("pkg-withdraw-risk", "v2", "release.A", "", ""); err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	old, _ := svc.Get("pkg-withdraw-risk", "v1")
	if old.Status != StatusRetired || old.SupersededBy != "v2" || old.RetiredCause != "SUPERSEDED" {
		t.Fatalf("旧版本状态 = %s / %s / %s", old.Status, old.SupersededBy, old.RetiredCause)
	}
	if act, _ := svc.Active("pkg-withdraw-risk"); act.PackageVersion != "v2" {
		t.Fatalf("生效版本 = %s", act.PackageVersion)
	}

	// 回滚：切回 v1，不产生新版本号
	rolled, err := svc.Rollback("pkg-withdraw-risk", "v1", "release.B", "线上误拒")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolled.PackageVersion != "v1" || rolled.Status != StatusPublished {
		t.Fatalf("回滚目标状态 = %+v", rolled)
	}
	if act, _ := svc.Active("pkg-withdraw-risk"); act.PackageVersion != "v1" {
		t.Fatalf("回滚后生效版本 = %s", act.PackageVersion)
	}
	bad, _ := svc.Get("pkg-withdraw-risk", "v2")
	if bad.Status != StatusRetired || bad.RetiredCause != "ROLLED_BACK" || bad.SupersededBy != "v1" {
		t.Fatalf("被回滚版本状态 = %s / %s", bad.Status, bad.RetiredCause)
	}
	if n := len(svc.List("pkg-withdraw-risk")); n != 2 {
		t.Fatalf("回滚不应产生新版本，版本数 = %d", n)
	}
}

// 验收 2：签名可校验，改一个字符即失效。
func TestSignatureDetectsTampering(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	v := mustDraft(t, svc, cat, "")

	if !Verify(v) {
		t.Fatal("刚构建的包应通过签名校验")
	}
	tampered := v.Clone()
	tampered.Package.Rulesets[0].Rules[0].Then.ReasonCode = "AMOUNT_LIMIT_EXCEEDED_2"
	if Verify(tampered) {
		t.Fatal("篡改后签名应失效")
	}
}

// 验收 3：任何一个规则集不合法，整包不构建（原子性）。
func TestBuildIsAllOrNothing(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)

	bad := defAmount(cat)
	bad.RulesetID = "rs-bad"
	bad.Rules[0].When = json.RawMessage(`{"op":"gt","args":[{"fact":"request.amout"},{"type":"decimal","value":"1"}]}`)

	_, reports, err := svc.CreateDraft(BuildInput{
		PackageID: "pkg-withdraw-risk",
		Rulesets:  []dsl.Definition{defAmount(cat), bad},
		Catalog:   cat,
		Actor:     "engineer.A",
	})
	var se *StateError
	if !errors.As(err, &se) || se.Code != CodeCompileFailed {
		t.Fatalf("期望 %s，得到 %v", CodeCompileFailed, err)
	}
	if len(reports) != 2 {
		t.Fatalf("应同时返回两个规则集的 dry-run 报告，实际 %d", len(reports))
	}
	if n := len(svc.List("pkg-withdraw-risk")); n != 0 {
		t.Fatalf("编译失败不应入库，实际 %d 个版本", n)
	}
}

// 产物就是交付给引擎的 RulePackage：字段齐全、IR 已解析成 slot。
func TestArtifactIsEngineReady(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	v := mustDraft(t, svc, cat, "")

	body, err := v.Artifact()
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	var pkg contracts.RulePackage
	if err := json.Unmarshal(body, &pkg); err != nil {
		t.Fatalf("产物不是合法 RulePackage: %v", err)
	}
	if pkg.PackageID != "pkg-withdraw-risk" || pkg.FactSchemaVersion != cat.CatalogVersion {
		t.Fatalf("包元数据不对: %+v", pkg)
	}
	if pkg.DSLVersion != "1.0" || pkg.IRVersion != "1.0" {
		t.Fatalf("版本字段不对: %s / %s", pkg.DSLVersion, pkg.IRVersion)
	}
	if len(pkg.Rulesets) != 1 || len(pkg.Rulesets[0].SlotTable) != 2 {
		t.Fatalf("规则集/slot 表不对: %+v", pkg.Rulesets)
	}
	if string(pkg.Rulesets[0].Rules[0].When) == "" || contains(string(pkg.Rulesets[0].Rules[0].When), `"fact"`) {
		t.Fatalf("when 应已解析成 slot: %s", pkg.Rulesets[0].Rules[0].When)
	}
	if pkg.Signature == "" || pkg.Signature != v.Signature {
		t.Fatalf("签名缺失或不一致: %s / %s", pkg.Signature, v.Signature)
	}
}

// 定时发布：published 但未到 validFrom 时不生效。
func TestScheduledPublish(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "v1")
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "2026-12-01T00:00:00Z", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	v, _ := svc.Active("pkg-withdraw-risk")
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if v.IsEffective(now) {
		t.Fatal("未到 validFrom 不应生效")
	}
	if !v.IsEffective(time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("过了 validFrom 应生效")
	}
}

func TestNextVersion(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "v1"},
		{[]string{"v1"}, "v2"},
		{[]string{"v1", "v9", "v3"}, "v10"},
		{[]string{"1.2.3", "1.2.10"}, "1.2.11"},
		{[]string{"1.9.0", "2.0.0"}, "2.0.1"},
	}
	for _, c := range cases {
		if got := NextVersion(c.in); got != c.want {
			t.Fatalf("NextVersion(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
