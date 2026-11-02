package dsl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goldenrules/platform/contracts"
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

func loadFixturePackage(t *testing.T) contracts.RulePackage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "rule-package.json"))
	if err != nil {
		t.Fatalf("读 fixtures 失败: %v", err)
	}
	var pkg contracts.RulePackage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("解析 fixtures 失败: %v", err)
	}
	return pkg
}

func baseDef() Definition {
	return Definition{
		RulesetID:       "rs-amount-limit",
		Scene:           "withdraw-risk-check",
		Version:         "rs-1",
		HitStrategy:     "ALL",
		DefaultDecision: "REJECT",
		Rules: []contracts.Rule{{
			RuleID:   "R1",
			Priority: 100,
			When:     json.RawMessage(`{"op":"gt","args":[{"fact":"request.amount"},{"type":"decimal","value":"1000"}]}`),
			Then:     contracts.Then{Decision: "REJECT", ReasonCode: "AMOUNT_LIMIT_EXCEEDED"},
		}},
	}
}

// crossDef is a 3x2 matrix with one cleared cell: enough rows to prove the traversal order, plus a gap in the middle.
func crossDef() Definition {
	return Definition{
		RulesetID:       "rs-cross-rate",
		Scene:           "withdraw-rate",
		Version:         "rs-1",
		HitStrategy:     "FIRST",
		DefaultDecision: "REJECT",
		CrossTable: &CrossTable{
			CrossTableID: "CT-001",
			RowHeaders:   []TableColumn{{Fact: "request.channel", Type: "enum"}},
			ColHeaders:   []TableColumn{{Fact: "sanctions.hit", Type: "boolean"}},
			RowCells: []map[string]json.RawMessage{
				{"request.channel": json.RawMessage(`"APP"`)},
				{"request.channel": json.RawMessage(`"WEB"`)},
				{"request.channel": json.RawMessage(`"API"`)},
			},
			ColCells: []map[string]json.RawMessage{
				{"sanctions.hit": json.RawMessage(`true`)},
				{"sanctions.hit": json.RawMessage(`false`)},
			},
			Cells: [][]json.RawMessage{
				{json.RawMessage(`{"type":"decimal","value":"0.10"}`), json.RawMessage(`{"type":"decimal","value":"0.12"}`)},
				{json.RawMessage(`{"type":"decimal","value":"0.15"}`), nil},
				{json.RawMessage(`{"type":"decimal","value":"0.20"}`), json.RawMessage(`{"type":"decimal","value":"0.22"}`)},
			},
			Target: CrossTarget{Var: "rate", Type: "decimal"},
			Then:   contracts.Then{Decision: "APPROVE", ReasonCode: "AMOUNT_RATE_MATCHED"},
		},
		OutputSchema: &contracts.OutputSchema{
			OutputSchemaID:      "withdraw-rate-output",
			OutputSchemaVersion: "v1",
			Fields:              map[string]contracts.OutputField{"rate": {Type: "decimal", Nullable: true}},
		},
	}
}

func analyze(t *testing.T, def Definition, cat contracts.FactCatalog, out *contracts.OutputSchema) Report {
	t.Helper()
	return Analyze(Input{Definition: def, Catalog: cat, OutputSchema: out, Limits: DefaultLimits()})
}

func hasDiag(list []Diagnostic, code string) *Diagnostic {
	for i := range list {
		if list[i].Code == code {
			return &list[i]
		}
	}
	return nil
}

// Acceptance 1: referencing a non-existent field → DSL_FIELD_NOT_FOUND + precise path + spelling suggestion.
func TestFieldNotFoundHasPrecisePath(t *testing.T) {
	cat := loadCatalog(t)
	def := baseDef()
	def.Rules[0].When = json.RawMessage(`{"op":"and","args":[
		{"op":"gt","args":[{"fact":"request.amount"},{"type":"decimal","value":"1000"}]},
		{"op":"eq","args":[{"fact":"request.amout"},{"type":"string","value":"CNY"}]}
	]}`)

	rep := analyze(t, def, cat, nil)
	if rep.Valid {
		t.Fatal("应判定不合法")
	}
	d := hasDiag(rep.Errors, CodeFieldNotFound)
	if d == nil {
		t.Fatalf("期望 %s，实际 %+v", CodeFieldNotFound, rep.Errors)
	}
	if d.Path != "rules[0].when.args[1].args[0]" {
		t.Fatalf("路径不精确: %s", d.Path)
	}
	if d.Fact != "request.amout" {
		t.Fatalf("fact = %q", d.Fact)
	}
	if d.Suggest == "" || !containsStr(d.Suggest, "request.amount") {
		t.Fatalf("缺少拼写建议: %q", d.Suggest)
	}
	if rep.IR != nil {
		t.Fatal("不合法时不应产出 IR")
	}
}

// Acceptance 2: a non-allow-listed operator → DSL_OP_NOT_ALLOWED.
func TestUnknownOpRejected(t *testing.T) {
	cat := loadCatalog(t)
	def := baseDef()
	def.Rules[0].When = json.RawMessage(`{"op":"startsWith","args":[{"fact":"request.currency"},"C"]}`)

	rep := analyze(t, def, cat, nil)
	d := hasDiag(rep.Errors, CodeOpNotAllowed)
	if d == nil {
		t.Fatalf("期望 %s，实际 %+v", CodeOpNotAllowed, rep.Errors)
	}
	if d.Path != "rules[0].when.op" {
		t.Fatalf("路径 = %s", d.Path)
	}
}

// Acceptance 3: textual expression sugar and the structured form produce **the same IR** (byte-identical).
func TestTextSugarEqualsStructured(t *testing.T) {
	cat := loadCatalog(t)

	structured := baseDef()
	structured.Rules[0].When = json.RawMessage(`{"op":"and","args":[
		{"op":"gt","args":[{"fact":"request.amount"},{"type":"decimal","value":"1000"}]},
		{"op":"eq","args":[{"fact":"request.currency"},{"type":"string","value":"CNY"}]}
	]}`)

	sugar := baseDef()
	sugar.Rules[0].When = json.RawMessage(`"request.amount > 1000 && request.currency == \"CNY\""`)

	a := analyze(t, structured, cat, nil)
	b := analyze(t, sugar, cat, nil)
	if !a.Valid || !b.Valid {
		t.Fatalf("两份都应合法: %+v / %+v", a.Errors, b.Errors)
	}
	ja, _ := json.Marshal(a.IR)
	jb, _ := json.Marshal(b.IR)
	if string(ja) != string(jb) {
		t.Fatalf("语法糖与结构化 IR 不一致:\n%s\n%s", ja, jb)
	}
	if a.SourceHash != b.SourceHash {
		t.Fatalf("sourceHash 不一致: %s / %s", a.SourceHash, b.SourceHash)
	}
}

// Acceptance 4: a decision table expands into equivalent rules and compiles into an IR.
func TestDecisionTableExpands(t *testing.T) {
	cat := loadCatalog(t)
	def := Definition{
		RulesetID:       "rs-withdraw-rate",
		Scene:           "withdraw-rate",
		HitStrategy:     "FIRST",
		DefaultDecision: "REJECT",
		Table: &Table{
			TableID: "DT-001",
			Columns: []TableColumn{
				{Fact: "request.channel", Type: "enum"},
				{Fact: "request.amount", Type: "decimal"},
			},
			Rows: []TableRow{
				{
					Conditions: map[string]json.RawMessage{
						"request.channel": json.RawMessage(`"APP"`),
						"request.amount":  json.RawMessage(`{"op":"lte","value":"1000"}`),
					},
					Then: contracts.Then{Decision: "APPROVE", ReasonCode: "OK",
						Assign: []contracts.Assign{{Var: "rate", Expr: json.RawMessage(`{"type":"decimal","value":"0.10"}`)}}},
				},
				{
					Conditions: map[string]json.RawMessage{
						"request.channel": json.RawMessage(`"APP"`),
						"request.amount":  json.RawMessage(`{"op":"gt","value":"1000"}`),
					},
					Then: contracts.Then{Decision: "REVIEW", ReasonCode: "AMOUNT_REVIEW"},
				},
			},
		},
		OutputSchema: &contracts.OutputSchema{
			OutputSchemaID:      "withdraw-risk-output",
			OutputSchemaVersion: "v1",
			Fields: map[string]contracts.OutputField{
				"rate": {Type: "decimal", Nullable: true},
			},
		},
	}

	rep := analyze(t, def, cat, nil)
	if !rep.Valid {
		t.Fatalf("决策表应可编译: %+v", rep.Errors)
	}
	if len(rep.IR.Rules) != 2 {
		t.Fatalf("行数应等于规则数，实际 %d", len(rep.IR.Rules))
	}
	if rep.IR.Rules[0].RuleID != "DT-001-R1" || rep.IR.Rules[1].RuleID != "DT-001-R2" {
		t.Fatalf("行序/ID 不对: %s %s", rep.IR.Rules[0].RuleID, rep.IR.Rules[1].RuleID)
	}
	if rep.IR.Rules[0].Priority <= rep.IR.Rules[1].Priority {
		t.Fatalf("行序应表达优先级: %d vs %d", rep.IR.Rules[0].Priority, rep.IR.Rules[1].Priority)
	}
	// after expansion, when is already resolved into slots and no longer contains fact names
	if containsStr(string(rep.IR.Rules[0].When), "fact") {
		t.Fatalf("编译后不应残留 fact 引用: %s", rep.IR.Rules[0].When)
	}
	if len(rep.IR.SlotTable) != 2 {
		t.Fatalf("slot 数 = %d (%+v)", len(rep.IR.SlotTable), rep.IR.SlotTable)
	}
}

// Acceptance 5: a cross table (decision matrix) expands cell by cell into ordinary rules.
func TestCrossTableExpands(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	rep := analyze(t, def, cat, nil)
	if !rep.Valid {
		t.Fatalf("交叉表应可编译: %+v", rep.Errors)
	}
	// 3 rows x 2 cols with one cleared cell → 5 rules, in row-major order.
	want := []string{"CT-001-R1C1", "CT-001-R1C2", "CT-001-R2C1", "CT-001-R3C1", "CT-001-R3C2"}
	if len(rep.IR.Rules) != len(want) {
		t.Fatalf("规则数 = %d，期望 %d", len(rep.IR.Rules), len(want))
	}
	for i, id := range want {
		if rep.IR.Rules[i].RuleID != id {
			t.Fatalf("第 %d 条 ID = %s，期望 %s", i, rep.IR.Rules[i].RuleID, id)
		}
	}
	// Priority counts every cell including the empty one, so clearing a cell does not renumber its neighbours:
	// R2C2 is the gap between 40 and 20.
	wantPrio := []int64{60, 50, 40, 20, 10}
	for i, r := range rep.IR.Rules {
		if r.Priority != wantPrio[i] {
			t.Fatalf("%s 优先级 = %d，期望 %d", r.RuleID, r.Priority, wantPrio[i])
		}
	}
	if rep.IR.Rules[0].Then.Decision != "APPROVE" || rep.IR.Rules[0].Then.ReasonCode != "AMOUNT_RATE_MATCHED" {
		t.Fatalf("表级结论应下发到每条规则: %+v", rep.IR.Rules[0].Then)
	}
	if len(rep.IR.Rules[0].Then.Assign) != 1 || rep.IR.Rules[0].Then.Assign[0].Var != "rate" {
		t.Fatalf("每格应赋值给 target.var: %+v", rep.IR.Rules[0].Then.Assign)
	}
	if !containsStr(string(rep.IR.Rules[0].Then.Assign[0].Expr), "0.10") {
		t.Fatalf("赋值表达式应带格子值: %s", rep.IR.Rules[0].Then.Assign[0].Expr)
	}
	// Both axes are and-ed into one condition, then resolved into slots at compile time.
	if containsStr(string(rep.IR.Rules[0].When), "fact") {
		t.Fatalf("编译后不应残留 fact 引用: %s", rep.IR.Rules[0].When)
	}
	if len(rep.IR.SlotTable) != 2 {
		t.Fatalf("slot 数 = %d (%+v)", len(rep.IR.SlotTable), rep.IR.SlotTable)
	}
}

// The same fact on both axes can never match; catching it at dry-run is the whole point of the shape check.
func TestCrossTableRejectsSharedFactAcrossAxes(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	def.CrossTable.ColHeaders = []TableColumn{{Fact: "request.channel", Type: "enum"}}

	rep := analyze(t, def, cat, nil)
	if rep.Valid {
		t.Fatal("同一事实同时作纵横维应判定不合法")
	}
	if d := hasDiag(rep.Errors, CodeTableInvalid); d == nil {
		t.Fatalf("期望 %s，实际 %+v", CodeTableInvalid, rep.Errors)
	}
}

// A wildcard in a dimension cell means "this axis does not constrain", which is a flat table in disguise.
func TestCrossTableRejectsWildcardInAxis(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	def.CrossTable.ColCells[1]["sanctions.hit"] = json.RawMessage(`"*"`)

	rep := analyze(t, def, cat, nil)
	if rep.Valid {
		t.Fatal("维度条件出现通配应判定不合法")
	}
	if d := hasDiag(rep.Errors, CodeTableInvalid); d == nil || !containsStr(d.Message, "通配") {
		t.Fatalf("期望通配错误，实际 %+v", rep.Errors)
	}
}

// Shape drift is the dangerous failure: cells and headers silently shift and every value lands in the wrong square.
func TestCrossTableRejectsShapeMismatch(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	def.CrossTable.Cells = def.CrossTable.Cells[:2]

	rep := analyze(t, def, cat, nil)
	if rep.Valid {
		t.Fatal("cells 行数与纵向条件不一致应判定不合法")
	}
	d := hasDiag(rep.Errors, CodeTableInvalid)
	if d == nil || !containsStr(d.Message, "不一致") {
		t.Fatalf("期望形状错误，实际 %+v", rep.Errors)
	}
}

// Duplicate conditions inside one axis produce a dead line.
func TestCrossTableRejectsDuplicateAxisValue(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	def.CrossTable.RowCells[1]["request.channel"] = json.RawMessage(`"APP"`)

	rep := analyze(t, def, cat, nil)
	if rep.Valid {
		t.Fatal("同一维度重复条件应判定不合法")
	}
	if d := hasDiag(rep.Errors, CodeTableInvalid); d == nil || !containsStr(d.Message, "完全相同") {
		t.Fatalf("期望重复条件错误，实际 %+v", rep.Errors)
	}
}

// Two lines may repeat one fact as long as another header fact distinguishes them - that is a legitimate matrix.
func TestCrossTableAllowsPartialRepeatWithTwoDimensions(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	def.CrossTable.RowHeaders = []TableColumn{
		{Fact: "request.channel", Type: "enum"},
		{Fact: "customer.level", Type: "enum"},
	}
	for i := range def.CrossTable.RowCells {
		level := []string{"NORMAL", "VIP", "NORMAL"}[i]
		def.CrossTable.RowCells[i]["customer.level"] = json.RawMessage(`"` + level + `"`)
	}

	rep := analyze(t, def, cat, nil)
	if !rep.Valid {
		t.Fatalf("只在同一事实重复、另一维度不同，应当合法: %+v", rep.Errors)
	}
}

// A matrix has no structural mutual exclusion, so anything but FIRST must at least warn.
func TestCrossTableWarnsWithoutFirst(t *testing.T) {
	cat := loadCatalog(t)
	def := crossDef()
	def.HitStrategy = ""

	rep := analyze(t, def, cat, nil)
	if !rep.Valid {
		t.Fatalf("应仍然合法（只是告警）: %+v", rep.Errors)
	}
	found := false
	for _, w := range rep.Warnings {
		if w.Code == CodeTableInvalid && containsStr(w.Message, "FIRST") {
			found = true
		}
	}
	if !found {
		t.Fatalf("期望命中策略告警，实际 %+v", rep.Warnings)
	}
}

// Enum literals must fall inside the value range.
func TestEnumLiteralChecked(t *testing.T) {
	cat := loadCatalog(t)
	def := baseDef()
	def.Rules[0].When = json.RawMessage(`{"op":"eq","args":[{"fact":"request.currency"},{"type":"string","value":"JPY"}]}`)

	rep := analyze(t, def, cat, nil)
	d := hasDiag(rep.Errors, CodeEnumInvalid)
	if d == nil {
		t.Fatalf("期望 %s，实际 %+v", CodeEnumInvalid, rep.Errors)
	}
	if !containsStr(d.Suggest, "CNY") {
		t.Fatalf("建议应列出合法值: %q", d.Suggest)
	}
}

// A nullable field without a guard → blocked; with an is null guard it is allowed through.
func TestNullableRequiresGuard(t *testing.T) {
	cat := loadCatalog(t)
	def := baseDef()
	def.Rules[0].When = json.RawMessage(`{"op":"lt","args":[{"fact":"customer.riskScore"},{"type":"integer","value":600}]}`)

	rep := analyze(t, def, cat, nil)
	if hasDiag(rep.Errors, CodeNullableUnguarded) == nil {
		t.Fatalf("期望 %s，实际 %+v", CodeNullableUnguarded, rep.Errors)
	}

	def.Rules[0].When = json.RawMessage(`{"op":"or","args":[
		{"op":"isNull","args":[{"fact":"customer.riskScore"}]},
		{"op":"lt","args":[{"fact":"customer.riskScore"},{"type":"integer","value":600}]}
	]}`)
	rep = analyze(t, def, cat, nil)
	if !rep.Valid {
		t.Fatalf("判空后应放行: %+v", rep.Errors)
	}
}

// assign variables must be registered in the scene output schema, and their types must match.
func TestAssignVarMustBeRegistered(t *testing.T) {
	cat := loadCatalog(t)
	def := baseDef()
	def.Rules[0].Then.Assign = []contracts.Assign{
		{Var: "riskLevel", Expr: json.RawMessage(`{"type":"enum","value":"HIGH"}`)},
	}
	out := &contracts.OutputSchema{
		OutputSchemaID:      "withdraw-risk-output",
		OutputSchemaVersion: "v1",
		Fields: map[string]contracts.OutputField{
			"riskLevel": {Type: "enum", Values: []string{"LOW", "MEDIUM", "HIGH"}},
			"rate":      {Type: "decimal"},
		},
	}
	rep := analyze(t, def, cat, out)
	if !rep.Valid {
		t.Fatalf("已登记变量应放行: %+v", rep.Errors)
	}

	def.Rules[0].Then.Assign = []contracts.Assign{
		{Var: "notRegistered", Expr: json.RawMessage(`{"type":"string","value":"x"}`)},
	}
	rep = analyze(t, def, cat, out)
	if hasDiag(rep.Errors, CodeOutputNotRegistered) == nil {
		t.Fatalf("期望 %s，实际 %+v", CodeOutputNotRegistered, rep.Errors)
	}
}

// Slots are assigned by (priority desc, ruleId asc), independent of authoring order.
func TestSlotAssignmentIsOrderIndependent(t *testing.T) {
	cat := loadCatalog(t)
	r1 := contracts.Rule{
		RuleID: "R1", Priority: 100,
		When: json.RawMessage(`{"op":"gt","args":[{"fact":"request.amount"},{"type":"decimal","value":"1000"}]}`),
		Then: contracts.Then{Decision: "REJECT", ReasonCode: "AMOUNT_LIMIT_EXCEEDED"},
	}
	r2 := contracts.Rule{
		RuleID: "R2", Priority: 90,
		When: json.RawMessage(`{"op":"eq","args":[{"fact":"request.currency"},{"type":"string","value":"CNY"}]}`),
		Then: contracts.Then{Decision: "APPROVE", ReasonCode: "OK"},
	}

	a := baseDef()
	a.Rules = []contracts.Rule{r1, r2}
	b := baseDef()
	b.Rules = []contracts.Rule{r2, r1} // reversed on purpose

	ra := analyze(t, a, cat, nil)
	rb := analyze(t, b, cat, nil)
	if !ra.Valid || !rb.Valid {
		t.Fatalf("两份都应合法: %+v / %+v", ra.Errors, rb.Errors)
	}
	if len(ra.IR.SlotTable) != 2 || ra.IR.SlotTable[0].Name != "request.amount" {
		t.Fatalf("slot 顺序应按优先级: %+v", ra.IR.SlotTable)
	}
	ja, _ := json.Marshal(ra.IR)
	jb, _ := json.Marshal(rb.IR)
	if string(ja) != string(jb) {
		t.Fatal("书写顺序不应影响 IR 字节")
	}
}

// The four rule sets in fixtures must all pass dry-run — the contract samples are the only cross-language referee.
func TestFixtureRulesetsAllValid(t *testing.T) {
	cat := loadCatalog(t)
	pkg := loadFixturePackage(t)

	for _, rs := range pkg.Rulesets {
		def := Definition{
			RulesetID:           rs.RulesetID,
			Scene:               rs.Scene,
			Version:             rs.Version,
			HitStrategy:         rs.HitStrategy,
			DefaultDecision:     rs.DefaultDecision,
			OutputSchemaID:      rs.OutputSchemaID,
			OutputSchemaVersion: rs.OutputSchemaVersion,
			Rules:               rs.Rules,
		}
		for i := range pkg.OutputSchemas {
			s := pkg.OutputSchemas[i]
			if s.OutputSchemaID == rs.OutputSchemaID {
				def.OutputSchema = &s
			}
		}
		// only take the test cases of the same scene
		for _, tc := range pkg.Tests {
			if tc.Scene == rs.Scene {
				def.Tests = append(def.Tests, tc)
			}
		}
		rep := analyze(t, def, cat, nil)
		if !rep.Valid {
			t.Fatalf("%s 校验失败: %+v", rs.RulesetID, rep.Errors)
		}
		if rep.Stats.Slots != len(rs.SlotTable) {
			t.Logf("%s: 重编译后 slot 数 %d（fixture 里是 %d，顺序可能不同）",
				rs.RulesetID, rep.Stats.Slots, len(rs.SlotTable))
		}
	}
}

// Operator precedence and parentheses of textual expressions.
func TestTextParserPrecedence(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{ // && binds tighter than ||; comparisons bind tighter than &&
			`a.b > 1 && c.d == "X" || e.f == true`,
			`{"args":[{"args":[{"args":[{"fact":"a.b"},{"type":"integer","value":1}],"op":"gt"},` +
				`{"args":[{"fact":"c.d"},{"type":"string","value":"X"}],"op":"eq"}],"op":"and"},` +
				`{"args":[{"fact":"e.f"},{"type":"boolean","value":true}],"op":"eq"}],"op":"or"}`,
		},
		{ // * binds tighter than +; parentheses win
			`a.b + 1 * 2 > 3`,
			`{"args":[{"args":[{"fact":"a.b"},{"args":[{"type":"integer","value":1},` +
				`{"type":"integer","value":2}],"op":"mul"}],"op":"add"},{"type":"integer","value":3}],"op":"gt"}`,
		},
		{ // is null / is not null
			`a.b is not null`,
			`{"args":[{"fact":"a.b"}],"negated":true,"op":"isNull"}`,
		},
		{ // in-list
			`a.b in ["X","Y"]`,
			`{"args":[{"fact":"a.b"},{"type":"array","value":["X","Y"]}],"op":"in"}`,
		},
	}
	for _, c := range cases {
		e, err := parseText(c.src)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", c.src, err)
		}
		got, _ := json.Marshal(e.JSON())
		if string(got) != c.want {
			t.Fatalf("结构不符:\n got %s\nwant %s", got, c.want)
		}
	}
}

func TestTextParserRejectsBadSyntax(t *testing.T) {
	bad := []string{
		`request.amount > `,
		`(request.amount > 1`,
		`request.amount > 1 > 2`,
		`request.amount @@ 1`,
		`upper(request.currency) == "A"`, // not in the whitelist
	}
	for _, src := range bad {
		if _, err := parseText(src); err == nil {
			t.Fatalf("应解析失败: %s", src)
		}
	}
}

func containsStr(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// RuleCount is what the rule-set list page shows. Table forms keep `rules: []` in storage, so a client that
// counts `len(rules)` reports 0 for a table that compiles to several rules - the list page did exactly that.
func TestRuleCountExpandsTableForms(t *testing.T) {
	cross := crossDef()
	if got := cross.RuleCount(); got != 5 { // 3 rows x 2 cols, one empty cell
		t.Fatalf("交叉表规则数 = %d, want 5", got)
	}

	plain := Definition{
		RulesetID:       "rs-table-count",
		Scene:           "withdraw-rate",
		DefaultDecision: "REJECT",
		Table: &Table{
			TableID: "DT-1",
			Columns: []TableColumn{{Fact: "request.channel", Type: "enum"}},
			Rows: []TableRow{
				{Conditions: map[string]json.RawMessage{"request.channel": json.RawMessage(`"APP"`)}},
				{Conditions: map[string]json.RawMessage{"request.channel": json.RawMessage(`"WEB"`)}},
			},
		},
	}
	if got := plain.RuleCount(); got != 2 {
		t.Fatalf("决策表规则数 = %d, want 2", got)
	}

	expr := Definition{DefaultDecision: "REJECT", Rules: []contracts.Rule{
		{RuleID: "R1", Priority: 10, When: json.RawMessage(`{"op":"eq","args":[{"fact":"sanctions.hit"},true]}`)},
	}}
	if got := expr.RuleCount(); got != 1 {
		t.Fatalf("表达式规则数 = %d, want 1", got)
	}
}
