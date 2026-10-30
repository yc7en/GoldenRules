package facts

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/persist"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	st, err := persist.Open(filepath.Join(t.TempDir(), "facts.json"), func() Catalog {
		return Catalog{CatalogVersion: CatalogVersion(time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), 0)}
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return New(st).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 11, 30, 0, 0, time.UTC)
	})
}

func amountFact() contracts.FactSchema {
	return contracts.FactSchema{
		Fact:        "request.amount",
		DisplayName: "本次提款金额",
		Type:        "decimal",
		Unit:        "CNY",
		Semantics:   "本次请求的提款本金，精确十进制",
		Required:    true,
		Consistency: "STRONG",
		Sensitivity: "PII",
		Version:     "v1",
		OnMissing:   "ERROR",
	}
}

func TestCreateThenGet(t *testing.T) {
	svc := newSvc(t)
	got, issues, err := svc.Create("admin", amountFact())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if HasError(issues) {
		t.Fatalf("unexpected errors: %+v", issues)
	}
	if got.Fact != "request.amount" {
		t.Fatalf("fact = %q", got.Fact)
	}
	if cat := svc.Catalog(); cat.Revision != 1 || cat.CatalogVersion != "catalog-v2026.10.1" {
		t.Fatalf("catalog = %+v", cat)
	}
	if _, ok := svc.Get("request.amount"); !ok {
		t.Fatal("未取到刚创建的事实")
	}
}

func TestValidationRules(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*contracts.FactSchema)
		wantCode string
	}{
		{"非法事实名", func(f *contracts.FactSchema) { f.Fact = "Request.Amount" }, CodeNameInvalid},
		{"非法类型", func(f *contracts.FactSchema) { f.Type = "float" }, CodeTypeInvalid},
		{"必填与默认互斥", func(f *contracts.FactSchema) {
			f.HasDefault = true
			f.Default = json.RawMessage(`"100"`)
		}, CodeRequiredDefault},
		{"默认值为 null", func(f *contracts.FactSchema) {
			f.Required = false
			f.HasDefault = true
			f.Default = json.RawMessage(`null`)
		}, CodeDefaultNull},
		{"未声明 hasDefault 却给 default", func(f *contracts.FactSchema) {
			f.Required = false
			f.Default = json.RawMessage(`"100"`)
		}, CodeDefaultNotAllowed},
		{"enum 缺枚举值", func(f *contracts.FactSchema) { f.Type = "enum" }, CodeEnumRequired},
		{"非 enum 带枚举值", func(f *contracts.FactSchema) { f.Enum = []string{"A"} }, CodeEnumNotAllowed},
		{"脱敏级别非法", func(f *contracts.FactSchema) { f.Sensitivity = "SECRET" }, CodeSensitivityInvalid},
		{"缺失策略非法", func(f *contracts.FactSchema) { f.OnMissing = "IGNORE" }, CodeOnMissingInvalid},
		{"版本号非法", func(f *contracts.FactSchema) { f.Version = "1" }, CodeVersionInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := amountFact()
			c.mutate(&in)
			issues := Validate(Normalize(in), nil)
			if !HasError(issues) {
				t.Fatalf("期望阻断，实际通过: %+v", issues)
			}
			if !hasCode(issues, c.wantCode) {
				t.Fatalf("期望错误码 %s，实际 %+v", c.wantCode, issues)
			}
		})
	}
}

func TestWarnDoesNotBlock(t *testing.T) {
	in := amountFact()
	in.Unit = "" // 金额缺 unit：只告警
	issues := Validate(Normalize(in), nil)
	if HasError(issues) {
		t.Fatalf("unit 缺失不应阻断: %+v", issues)
	}
	if !hasCode(issues, CodeUnitMissing) {
		t.Fatalf("期望 %s 告警，实际 %+v", CodeUnitMissing, issues)
	}
}

func TestDuplicateRejected(t *testing.T) {
	svc := newSvc(t)
	if _, _, err := svc.Create("admin", amountFact()); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, issues, err := svc.Create("admin", amountFact())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("期望 ValidationError，得到 %v", err)
	}
	if !hasCode(issues, CodeDuplicate) {
		t.Fatalf("期望 FACT_DUPLICATE，实际 %+v", issues)
	}
	if cat := svc.Catalog(); len(cat.Facts) != 1 {
		t.Fatalf("重复创建后事实数 = %d", len(cat.Facts))
	}
}

func TestUpdateBreakingChangeNeedsVersionBump(t *testing.T) {
	svc := newSvc(t)
	if _, _, err := svc.Create("admin", amountFact()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 改类型但不升版本 → 拒绝
	changed := amountFact()
	changed.Type = "integer"
	if _, issues, err := svc.Update("admin", "request.amount", changed); err == nil {
		t.Fatalf("改类型未升版本应被拒绝，实际 issues=%+v", issues)
	}

	// 升版本后允许
	changed.Version = "v2"
	got, _, err := svc.Update("admin", "request.amount", changed)
	if err != nil {
		t.Fatalf("升版本后应可改类型: %v", err)
	}
	if got.Type != "integer" || got.Version != "v2" {
		t.Fatalf("got = %+v", got)
	}
}

func TestUpdateCannotRename(t *testing.T) {
	svc := newSvc(t)
	if _, _, err := svc.Create("admin", amountFact()); err != nil {
		t.Fatalf("create: %v", err)
	}
	renamed := amountFact()
	renamed.Fact = "request.amt"
	if _, _, err := svc.Update("admin", "request.amount", renamed); err == nil {
		t.Fatal("改名应被拒绝")
	}
}

func TestDeleteBlockedWhenReferenced(t *testing.T) {
	svc := newSvc(t)
	if _, _, err := svc.Create("admin", amountFact()); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc.WithRefChecker(func(fact string) []string {
		if fact == "request.amount" {
			return []string{"rs-amount-limit"}
		}
		return nil
	})
	err := svc.Delete("admin", "request.amount")
	var inUse *InUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("期望 InUseError，得到 %v", err)
	}
	if len(inUse.Referers) != 1 {
		t.Fatalf("referers = %+v", inUse.Referers)
	}
	if _, ok := svc.Get("request.amount"); !ok {
		t.Fatal("被引用的事实不应被删除")
	}
}

func TestArtifactsIdempotentAndDrift(t *testing.T) {
	svc := newSvc(t)
	dir := t.TempDir()
	for _, f := range []contracts.FactSchema{
		amountFact(),
		{Fact: "customer.level", Type: "enum", Enum: []string{"NORMAL", "VIP"}, Required: true, Version: "v1"},
	} {
		if _, _, err := svc.Create("admin", f); err != nil {
			t.Fatalf("create %s: %v", f.Fact, err)
		}
	}

	changed, err := svc.Write(dir)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(changed) != 3 {
		t.Fatalf("首次生成应写 3 个文件，实际 %v", changed)
	}

	// 幂等：再写一次不应有任何改动
	changed, err = svc.Write(dir)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("幂等失败，重复生成改了 %v", changed)
	}

	drifts, err := svc.Verify(dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("刚生成不应过期: %+v", drifts)
	}

	// 改事实目录但不重新生成 → 必须被 CI 门禁抓到
	if _, _, err := svc.Create("admin", contracts.FactSchema{
		Fact: "sanctions.hit", Type: "boolean", Required: true, Version: "v1",
	}); err != nil {
		t.Fatalf("create sanctions: %v", err)
	}
	drifts, err = svc.Verify(dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(drifts) == 0 {
		t.Fatal("目录变更后生成物应判为过期")
	}
	for _, d := range drifts {
		if d.Code != CodeArtifactStale {
			t.Fatalf("期望 %s，实际 %+v", CodeArtifactStale, d)
		}
	}
}

func TestSlotTableDeterministic(t *testing.T) {
	svc := newSvc(t)
	for _, f := range []contracts.FactSchema{
		{Fact: "request.amount", Type: "decimal", Unit: "CNY", Required: true, Version: "v2"},
		{Fact: "customer.level", Type: "enum", Enum: []string{"NORMAL"}, Required: true, Version: "v1"},
	} {
		if _, _, err := svc.Create("admin", f); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	arts, err := svc.Artifacts()
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	if len(arts.SlotTable) != 2 {
		t.Fatalf("slot 数 = %d", len(arts.SlotTable))
	}
	// 按字段名升序：customer.level 在 request.amount 之前
	if arts.SlotTable[0].Name != "customer.level" || arts.SlotTable[1].Name != "request.amount" {
		t.Fatalf("slot 顺序不确定: %+v", arts.SlotTable)
	}
	if arts.SlotTable[1].Type != "decimal" || arts.SlotTable[1].FactVersion != "v2" {
		t.Fatalf("slot 内容错: %+v", arts.SlotTable[1])
	}
	f, ok := arts.File(FileJSONSchema)
	if !ok {
		t.Fatal("缺 fact-schema.json")
	}
	var doc map[string]any
	if err := json.Unmarshal(f.Body, &doc); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	if doc["x-catalog-version"] != "catalog-v2026.10.2" {
		t.Fatalf("x-catalog-version = %v", doc["x-catalog-version"])
	}
}

func hasCode(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}
