package dsl

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"goldenrules/platform/contracts"
)

// Limits 静态校验的资源上限（`contracts/01` §4.5）。
type Limits struct {
	MaxDepth      int
	MaxNodes      int
	MaxRulesPerRS int
	// StrictNullable 打开「可空字段必须显式判空」的阻断（`contracts/01` §5.2 #4）。
	StrictNullable bool
}

// DefaultLimits 契约默认值。
func DefaultLimits() Limits {
	return Limits{MaxDepth: 64, MaxNodes: 1000, MaxRulesPerRS: 1000, StrictNullable: true}
}

// Input 一次 dry-run 校验的输入。
type Input struct {
	Definition   Definition
	Catalog      contracts.FactCatalog
	OutputSchema *contracts.OutputSchema // 可为空：为空时只做「无法校验」的告警
	Limits       Limits
}

// Stats 编译统计（前端校验面板要显示）。
type Stats struct {
	Rules       int `json:"rules"`
	Slots       int `json:"slots"`
	MaxDepth    int `json:"maxDepth"`
	MaxNodes    int `json:"maxNodes"`
	AssignCount int `json:"assignCount"`
}

// Report 是 dry-run 校验结果。`Valid=false` 时 `IR` 为 nil（不产出半成品）。
type Report struct {
	RulesetID     string              `json:"rulesetId"`
	Valid         bool                `json:"valid"`
	Errors        []Diagnostic        `json:"errors"`
	Warnings      []Diagnostic        `json:"warnings"`
	Stats         Stats               `json:"stats"`
	IR            *contracts.RuleSet  `json:"ir,omitempty"`
	SlotTable     []contracts.SlotDef `json:"slotTable,omitempty"`
	SourceHash    string              `json:"sourceHash,omitempty"`
	SlotTableHash string              `json:"slotTableHash,omitempty"`
	CompiledAt    string              `json:"compiledAt,omitempty"`
}

type assignAST struct {
	Var  string
	Expr *Expr
}

type ruleAST struct {
	Rule    contracts.Rule
	When    *Expr
	Assigns []assignAST
}

var builtinDecisions = map[string]bool{"APPROVE": true, "REJECT": true, "REVIEW": true, "ROUTE": true}

var reasonPrefixes = []string{"AMOUNT_", "LIST_", "FREQ_", "CONTRACT_", "DEGRADE_", "DEFAULT_"}

// Analyze 是 B7 的 dry-run 主入口：解析 → 静态校验 → （无错时）编译出 IR 预览。
//
// 校验顺序刻意固定（结构 → 表展开 → 单规则语义 → 类型 → 输出变量 → 上限），
// 保证同一份输入永远得到同一批诊断，便于前端/agent 复现。
func Analyze(in Input) Report {
	lim := in.Limits
	if lim.MaxDepth == 0 {
		lim = DefaultLimits()
	}
	def := in.Definition
	d := &diag{}
	rep := Report{RulesetID: def.RulesetID}

	// ---- 1 规则集结构 ----
	if strings.TrimSpace(def.RulesetID) == "" {
		d.errorf(CodeRuleSetInvalid, "rulesetId", "", "", "规则集缺少 rulesetId")
	} else if !identPattern.MatchString(def.RulesetID) {
		d.errorf(CodeRuleSetInvalid, "rulesetId", "", "", "rulesetId 只能包含字母数字与 ._-")
	}
	if strings.TrimSpace(def.Scene) == "" {
		d.errorf(CodeRuleSetInvalid, "scene", "", "", "规则集缺少 scene（场景是运行时的入口键）")
	}
	if def.HitStrategy != "" && def.HitStrategy != "ALL" && def.HitStrategy != "FIRST" && def.HitStrategy != "PRIORITY" {
		d.errorf(CodeRuleSetInvalid, "hitStrategy", "", "",
			fmt.Sprintf("命中策略 %q 不合法", def.HitStrategy), "合法值：ALL（默认）/ FIRST / PRIORITY")
	}
	if def.DefaultDecision == "" {
		d.errorf(CodeRuleSetInvalid, "defaultDecision", "", "",
			"缺少 defaultDecision（全不命中时的兜底决策，contracts/01 §3.4.2）",
			"通常写 REJECT（fail-closed）")
	} else if !builtinDecisions[def.DefaultDecision] {
		d.errorf(CodeDecisionNotReg, "defaultDecision", "", "",
			fmt.Sprintf("defaultDecision %q 不在内置枚举内", def.DefaultDecision),
			"P0 只支持 APPROVE / REJECT / REVIEW / ROUTE")
	}
	if len(def.Rules) > 0 && def.Table != nil {
		d.errorf(CodeRuleSetInvalid, "", "", "",
			"同一规则集不能同时用「表达式」与「决策表」两种写法",
			"决策表/表达式是同一 IR 的两种写法，不是两个模块（contracts/01 §3.1）")
	}

	// ---- 2 写法展开 ----
	rules := def.Rules
	if def.Table != nil {
		rules = ExpandTable(def.Table, d)
	}
	if len(rules) == 0 && !d.hasError() {
		d.errorf(CodeRuleSetInvalid, "rules", "", "", "规则集里没有任何规则")
	}
	if len(rules) > lim.MaxRulesPerRS {
		d.errorf(CodeLimitExceeded, "rules", "", "",
			fmt.Sprintf("规则数 %d 超过单规则集上限 %d", len(rules), lim.MaxRulesPerRS),
			"拆成多个规则集，按场景挂载")
	}

	cat := make(map[string]contracts.FactSchema, len(in.Catalog.Facts))
	for _, f := range in.Catalog.Facts {
		cat[f.Fact] = f
	}
	names := make([]string, 0, len(cat))
	for k := range cat {
		names = append(names, k)
	}
	sort.Strings(names)

	// ---- 3 逐条规则 ----
	seenID := map[string]bool{}
	asts := make([]ruleAST, 0, len(rules))
	for i, r := range rules {
		path := fmt.Sprintf("rules[%d]", i)
		if strings.TrimSpace(r.RuleID) == "" {
			d.errorf(CodeRuleSetInvalid, path+".ruleId", "", "", "规则缺少 ruleId")
			continue
		}
		if seenID[r.RuleID] {
			d.errorf(CodeDuplicateRuleID, path+".ruleId", r.RuleID, "", "规则 ID 重复："+r.RuleID)
		}
		seenID[r.RuleID] = true
		if r.Scene != "" && def.Scene != "" && r.Scene != def.Scene {
			d.warnf(CodeRuleSetInvalid, path+".scene", r.RuleID, "",
				fmt.Sprintf("规则 scene %q 与规则集 scene %q 不一致，以规则集为准", r.Scene, def.Scene))
		}

		// then
		checkThen(d, r, path, in.OutputSchema)

		// when
		whenPath := path + ".when"
		e := ParseWhen(r.When, whenPath, d)
		if e == nil {
			continue
		}
		normalizeLiterals(e, cat)
		if e.Depth() > lim.MaxDepth {
			d.errorf(CodeLimitExceeded, whenPath, r.RuleID, "",
				fmt.Sprintf("表达式深度 %d 超过上限 %d", e.Depth(), lim.MaxDepth))
		}
		if e.NodeCount() > lim.MaxNodes {
			d.errorf(CodeLimitExceeded, whenPath, r.RuleID, "",
				fmt.Sprintf("表达式节点数 %d 超过上限 %d", e.NodeCount(), lim.MaxNodes))
		}
		c := &checker{d: d, cat: cat, names: names, ruleID: r.RuleID, strictNullable: lim.StrictNullable}
		guards := guardsOf(e)
		kind := c.check(e, whenPath, guards)
		if kind != "" && kind != "boolean" {
			d.errorf(CodeTypeMismatch, whenPath, r.RuleID, "",
				"when 必须返回布尔，实际是 "+kind, "比较/逻辑运算才能作为条件")
		}

		// assign
		var assigns []assignAST
		for j, a := range r.Then.Assign {
			apath := fmt.Sprintf("%s.then.assign[%d]", path, j)
			if a.Var == "" {
				d.errorf(CodeRuleSetInvalid, apath+".var", r.RuleID, "", "assign 缺少变量名")
				continue
			}
			if !outputVarPattern.MatchString(a.Var) {
				d.errorf(CodeRuleSetInvalid, apath+".var", r.RuleID, "",
					"变量名只能是字母/下划线开头的标识符："+a.Var)
			}
			ae := ParseValue(a.Expr, apath+".expr", d)
			if ae == nil {
				continue
			}
			normalizeLiterals(ae, cat)
			c.check(ae, apath+".expr", guards)
			if in.OutputSchema != nil {
				field, ok := in.OutputSchema.Fields[a.Var]
				if !ok {
					d.errorf(CodeOutputNotRegistered, apath+".var", r.RuleID, "",
						"输出变量未在场景输出 schema 登记："+a.Var,
						"登记到 "+in.OutputSchema.OutputSchemaID+" 后再用（contracts/03 §5.1）")
				} else if ak := kindOf(ae, cat); ak != "" && !kindMatches(field.Type, ak) {
					d.errorf(CodeTypeMismatch, apath+".expr", r.RuleID, "",
						fmt.Sprintf("变量 %s 声明为 %s，表达式却是 %s", a.Var, field.Type, ak))
				}
			} else if len(def.Tests) >= 0 {
				d.warnf(CodeOutputNotRegistered, apath+".var", r.RuleID, "",
					"未提供场景输出 schema，无法校验输出变量 "+a.Var+" 是否登记")
			}
			assigns = append(assigns, assignAST{Var: a.Var, Expr: ae})
		}

		asts = append(asts, ruleAST{Rule: r, When: e, Assigns: assigns})
	}

	// ---- 4 测试用例 ----
	for i, tc := range def.Tests {
		tpath := fmt.Sprintf("tests[%d]", i)
		if tc.TestID == "" {
			d.errorf(CodeTestInvalid, tpath+".testId", "", "", "测试用例缺少 testId")
		}
		if tc.Scene != "" && def.Scene != "" && tc.Scene != def.Scene {
			d.warnf(CodeTestInvalid, tpath+".scene", "", "", "用例 scene 与规则集不一致")
		}
		if !builtinDecisions[tc.Expect.Decision] {
			d.errorf(CodeDecisionNotReg, tpath+".expect.decision", "", "",
				fmt.Sprintf("期望决策 %q 不在内置枚举内", tc.Expect.Decision))
		}
		if tc.Expect.ReasonCode == "" {
			d.errorf(CodeReasonCodeInvalid, tpath+".expect.reasonCode", "", "", "期望结果缺 reasonCode")
		}
		for k := range tc.Input {
			if _, ok := cat[k]; !ok {
				d.warnf(CodeFieldNotFound, tpath+".input", "", k,
					"用例输入引用了目录里不存在的事实 "+k, "确认字段名或补进事实目录")
			}
		}
	}

	// ---- 5 汇总 ----
	rep.Errors, rep.Warnings = split(d.list)
	rep.Stats.Rules = len(asts)
	for _, a := range asts {
		if dep := a.When.Depth(); dep > rep.Stats.MaxDepth {
			rep.Stats.MaxDepth = dep
		}
		if n := a.When.NodeCount(); n > rep.Stats.MaxNodes {
			rep.Stats.MaxNodes = n
		}
		rep.Stats.AssignCount += len(a.Assigns)
	}
	if len(rep.Errors) > 0 {
		return rep
	}

	// ---- 6 编译（只有全绿才产出 IR）----
	ir, slots, sourceHash, slotHash := compile(def, asts, cat)
	rep.Valid = true
	rep.IR = ir
	rep.SlotTable = slots
	rep.SourceHash = sourceHash
	rep.SlotTableHash = slotHash
	rep.Stats.Slots = len(slots)
	return rep
}

func split(list []Diagnostic) (errs, warns []Diagnostic) {
	errs, warns = []Diagnostic{}, []Diagnostic{}
	for _, x := range list {
		if x.Level == "error" {
			errs = append(errs, x)
		} else {
			warns = append(warns, x)
		}
	}
	return errs, warns
}

// checkThen 校验命中后果：决策枚举、理由码、assign 结构。
func checkThen(d *diag, r contracts.Rule, path string, out *contracts.OutputSchema) {
	tpath := path + ".then"
	if r.Then.Decision == "" {
		d.errorf(CodeRuleSetInvalid, tpath+".decision", r.RuleID, "", "规则缺少 decision")
	} else if !builtinDecisions[r.Then.Decision] {
		d.errorf(CodeDecisionNotReg, tpath+".decision", r.RuleID, "",
			fmt.Sprintf("decision %q 不在内置枚举内", r.Then.Decision),
			"P0 只支持 APPROVE / REJECT / REVIEW / ROUTE；自定义值需先在注册中心登记（contracts/03 §3）")
	}
	if r.Then.ReasonCode == "" {
		d.errorf(CodeReasonCodeInvalid, tpath+".reasonCode", r.RuleID, "",
			"规则缺少 reasonCode（稳定枚举，客服/合规/报表依赖它）",
			"例如 AMOUNT_LIMIT_EXCEEDED")
	} else if !hasKnownReasonPrefix(r.Then.ReasonCode) {
		d.warnf(CodeReasonCodeInvalid, tpath+".reasonCode", r.RuleID, "",
			"理由码前缀未登记："+r.Then.ReasonCode,
			"已登记前缀："+strings.Join(reasonPrefixes, " / ")+"（或 OK）")
	}
	_ = out
}

func hasKnownReasonPrefix(code string) bool {
	if code == "OK" {
		return true
	}
	for _, p := range reasonPrefixes {
		if strings.HasPrefix(code, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 类型检查
// ---------------------------------------------------------------------------

type checker struct {
	d              *diag
	cat            map[string]contracts.FactSchema
	names          []string
	ruleID         string
	strictNullable bool
}

// guardsOf 收集「被显式判空」的字段：isNull(x) 与 coalesce(x, ..) 的第一个参数。
// 这是可空字段的合法用法，不能让「可空必须判空」的检查把它误判成违规。
func guardsOf(e *Expr) map[string]bool {
	out := map[string]bool{}
	e.Walk(func(n *Expr) {
		if n.Kind != KindOp {
			return
		}
		if n.Op == "isNull" && len(n.Args) == 1 && n.Args[0].Kind == KindFact {
			out[n.Args[0].Fact] = true
		}
		if n.IsCall() && (n.Func == "isNull" || n.Func == "coalesce") && len(n.Args) > 0 && n.Args[0].Kind == KindFact {
			out[n.Args[0].Fact] = true
		}
	})
	return out
}

// check 返回推断出的类型（"" = 未知，不继续报错以免噪声叠加）。
func (c *checker) check(e *Expr, path string, guards map[string]bool) string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case KindLiteral:
		return e.Lit.Kind
	case KindSlot:
		c.d.errorf(CodeSyntaxError, path, c.ruleID, "",
			"DSL 阶段不允许出现 slot 引用", "slot 是编译产物；请写 {\"fact\":\"namespace.field\"}")
		return ""
	case KindFact:
		f, ok := c.cat[e.Fact]
		if !ok {
			c.d.errorf(CodeFieldNotFound, path, c.ruleID, e.Fact,
				"引用的字段不在事实目录中："+e.Fact, c.suggest(e.Fact))
			return ""
		}
		if f.Deprecated {
			c.d.warnf(CodeDeprecatedField, path, c.ruleID, e.Fact,
				"引用了已废弃字段："+e.Fact+"（replacedBy: "+f.ReplacedBy+"）",
				"迁移到 "+f.ReplacedBy)
		}
		if f.Nullable && c.strictNullable && !guards[e.Fact] {
			c.d.errorf(CodeNullableUnguarded, path, c.ruleID, e.Fact,
				"可空字段未显式判空："+e.Fact,
				"用 is null / is not null 或 coalesce("+e.Fact+", ..) 兜底后再参与运算（contracts/01 §5.2 #4）")
		}
		return f.Type
	case KindOp:
		return c.checkOp(e, path, guards)
	}
	return ""
}

func (c *checker) checkOp(e *Expr, path string, guards map[string]bool) string {
	minA, maxA := OpArity(e.Op)
	if e.Op == "call" {
		minA, maxA = FuncArity(e.Func)
	}
	if len(e.Args) < minA || (maxA >= 0 && len(e.Args) > maxA) {
		label := e.Op
		if e.Op == "call" {
			label = e.Func + "()"
		}
		c.d.errorf(CodeTypeMismatch, path, c.ruleID, "",
			fmt.Sprintf("%s 的参数个数是 %d，要求 %s", label, len(e.Args), arityText(minA, maxA)))
		// 参数个数错时仍继续检查子树，一次性给全诊断
	}

	argKinds := make([]string, len(e.Args))
	for i, a := range e.Args {
		argKinds[i] = c.check(a, fmt.Sprintf("%s.args[%d]", path, i), guards)
	}

	if e.Op == "call" {
		return c.checkCall(e, path, argKinds)
	}

	switch e.Op {
	case "and", "or":
		for i, k := range argKinds {
			if k != "" && k != "boolean" {
				c.d.errorf(CodeTypeMismatch, fmt.Sprintf("%s.args[%d]", path, i), c.ruleID, "",
					fmt.Sprintf("%s 的参数必须是布尔，第 %d 个是 %s", e.Op, i+1, k))
			}
		}
		return "boolean"
	case "not":
		if len(argKinds) == 1 && argKinds[0] != "" && argKinds[0] != "boolean" {
			c.d.errorf(CodeTypeMismatch, path+".args[0]", c.ruleID, "",
				"not 的参数必须是布尔，实际是 "+argKinds[0])
		}
		return "boolean"
	case "isNull":
		return "boolean"
	case "eq", "ne":
		c.checkComparable(e, path, argKinds, false)
		return "boolean"
	case "gt", "gte", "lt", "lte":
		c.checkComparable(e, path, argKinds, true)
		return "boolean"
	case "add", "sub", "mul", "div", "mod":
		for i, k := range argKinds {
			if k != "" && !isNumeric(k) {
				c.d.errorf(CodeTypeMismatch, fmt.Sprintf("%s.args[%d]", path, i), c.ruleID, "",
					fmt.Sprintf("%s 的参数必须是数值，第 %d 个是 %s", e.Op, i+1, k))
			}
		}
		if anyDecimal(argKinds) {
			return "decimal"
		}
		if len(argKinds) > 0 && argKinds[0] != "" {
			return "integer"
		}
		return ""
	case "in", "notIn":
		if len(e.Args) == 2 {
			k := argKinds[0]
			lst := e.Args[1]
			if lst.Kind != KindLiteral || lst.Lit.Kind != "array" {
				c.d.errorf(CodeTypeMismatch, path+".args[1]", c.ruleID, "",
					e.Op+" 的右侧必须是字面量数组", `例如 {"op":"in","args":[{"fact":"x.y"},{"type":"array","value":["A","B"]}]}`)
				return "boolean"
			}
			c.checkEnumMembership(e.Args[0], lst.Lit.Value, path, guards)
			for i, item := range lst.Lit.Value.([]any) {
				ik := literalKindOf(item)
				if k != "" && ik != "" && !kindMatches(k, ik) {
					c.d.errorf(CodeTypeMismatch, fmt.Sprintf("%s.args[1].value[%d]", path, i), c.ruleID, "",
						fmt.Sprintf("集合元素类型 %s 与左侧 %s 不匹配", ik, k))
				}
			}
		}
		return "boolean"
	}
	return ""
}

func (c *checker) checkCall(e *Expr, path string, argKinds []string) string {
	switch e.Func {
	case "now":
		return "string" // datetime 以 RFC3339 字符串出现（contracts/02 §5）
	case "inSet", "contains", "isNull":
		return "boolean"
	case "stringLen":
		return "integer"
	case "toUpper", "toLower", "substr":
		return "string"
	case "abs", "min", "max", "round":
		if anyDecimal(argKinds) {
			return "decimal"
		}
		return "integer"
	case "coalesce":
		for _, k := range argKinds {
			if k != "" && k != "null" {
				return k
			}
		}
		return ""
	}
	return ""
}

// checkComparable 比较运算的类型兼容性 + 枚举字面量合法性。
func (c *checker) checkComparable(e *Expr, path string, argKinds []string, ordered bool) {
	if len(e.Args) != 2 || len(argKinds) != 2 {
		return
	}
	l, r := argKinds[0], argKinds[1]
	if l == "" || r == "" {
		c.checkEnumMembership(e.Args[0], literalValue(e.Args[1]), path, nil)
		return
	}
	if ordered {
		// 大小比较只允许数值或同类型有序值
		if !(isNumeric(l) && isNumeric(r)) && l != r {
			c.d.errorf(CodeTypeMismatch, path, c.ruleID, "",
				fmt.Sprintf("%s 两侧类型不可比较：%s 与 %s", e.Op, l, r),
				"大小比较要求两侧同为数值，或同为相同类型的值")
			return
		}
	} else if !kindMatches(l, r) {
		c.d.errorf(CodeTypeMismatch, path, c.ruleID, "",
			fmt.Sprintf("%s 两侧类型不匹配：%s 与 %s", e.Op, l, r),
			"比较两侧要么同类，要么同为数值（integer/decimal）；不做隐式跨类型转换")
		return
	}
	c.checkEnumMembership(e.Args[0], literalValue(e.Args[1]), path, nil)
	c.checkEnumMembership(e.Args[1], literalValue(e.Args[0]), path, nil)
}

// checkEnumMembership 枚举字段与字面量比较时，字面量必须在枚举值内。
func (c *checker) checkEnumMembership(factSide *Expr, lit any, path string, _ map[string]bool) {
	if factSide == nil || factSide.Kind != KindFact {
		return
	}
	f, ok := c.cat[factSide.Fact]
	if !ok || f.Type != "enum" || len(f.Enum) == 0 {
		return
	}
	s, ok := lit.(string)
	if !ok {
		return
	}
	for _, v := range f.Enum {
		if v == s {
			return
		}
	}
	c.d.errorf(CodeEnumInvalid, path, c.ruleID, f.Fact,
		fmt.Sprintf("枚举字面量 %q 不在 %s 的取值范围内", s, f.Fact),
		"合法值："+strings.Join(f.Enum, " / "))
}

func literalValue(e *Expr) any {
	if e == nil || e.Kind != KindLiteral {
		return nil
	}
	return e.Lit.Value
}

func literalKindOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case int64:
		return "integer"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return ""
}

func kindOf(e *Expr, cat map[string]contracts.FactSchema) string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case KindLiteral:
		return e.Lit.Kind
	case KindFact:
		return cat[e.Fact].Type
	}
	return ""
}

func isNumeric(k string) bool { return k == "integer" || k == "decimal" }

func anyDecimal(kinds []string) bool {
	for _, k := range kinds {
		if k == "decimal" {
			return true
		}
	}
	return false
}

// kindMatches 类型兼容：同类、数值互通、枚举与字符串互通。
func kindMatches(a, b string) bool {
	if a == b {
		return true
	}
	if isNumeric(a) && isNumeric(b) {
		return true
	}
	if (a == "enum" && b == "string") || (a == "string" && b == "enum") {
		return true
	}
	return false
}

func arityText(minA, maxA int) string {
	switch {
	case maxA < 0:
		return fmt.Sprintf("至少 %d 个", minA)
	case minA == maxA:
		return fmt.Sprintf("%d 个", minA)
	default:
		return fmt.Sprintf("%d~%d 个", minA, maxA)
	}
}

// suggest 字段名拼错时给最近候选（编辑距离 ≤ 3）。
func (c *checker) suggest(name string) string {
	best, bestDist := "", 4
	for _, n := range c.names {
		if d := editDistance(name, n); d < bestDist {
			best, bestDist = n, d
		}
	}
	if best == "" {
		return "检查字段名，或在事实目录里登记该字段"
	}
	return "是否拼写错误？目录中相近字段：" + best
}

// editDistance Levenshtein 距离。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// RawJSON 便于把 AST 转回 RawMessage（编译层用）。
func RawJSON(e *Expr) json.RawMessage { return mustJSON(e) }

// ---------------------------------------------------------------------------
// 字面量类型归一
// ---------------------------------------------------------------------------

// normalizeLiterals 把「与 decimal 字段比较的整数字面量」提升为 decimal。
//
// 这是 `contracts/01` §7.3 的要求：`{"fact":"request.amount"} > 1000` 编译后是
// `{"slot":3} > {"type":"decimal","value":"1000"}`。它不是隐式跨类型转换——事实之间
// 依然不许混算，只是**字面量**按对侧字段类型落地，这样文本糖与结构化两种写法能编译出
// 字节一致的 IR（§5.3）。
func normalizeLiterals(e *Expr, cat map[string]contracts.FactSchema) {
	if e == nil {
		return
	}
	for _, a := range e.Args {
		normalizeLiterals(a, cat)
	}
	if e.Kind != KindOp || len(e.Args) != 2 {
		return
	}
	switch e.Op {
	case "eq", "ne", "gt", "gte", "lt", "lte", "add", "sub", "mul", "div", "mod", "in", "notIn":
	default:
		return
	}
	for i := 0; i < 2; i++ {
		factSide, litSide := e.Args[i], e.Args[1-i]
		if factSide.Kind != KindFact || litSide.Kind != KindLiteral {
			continue
		}
		if litSide.Lit.Kind != "integer" {
			continue
		}
		if cat[factSide.Fact].Type != "decimal" {
			continue
		}
		if i64, ok := litSide.Lit.Value.(int64); ok {
			litSide.Lit = &Literal{Kind: "decimal", Value: fmt.Sprintf("%d", i64)}
		}
	}
}
