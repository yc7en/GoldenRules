// Package dsl 实现 B7：规则 DSL 的解析、校验与编译（dry-run）。
//
// 契约依据：`contracts/01`（DSL 与 IR 规范）、`contracts/05` §6（校验流水线）。
//
// # 三种写法，一套 IR
//
// 结构化 JSON、文本表达式糖、决策表，三者**表达等价**，都先收敛到本包的 [Expr] AST，
// 再编译成同一份 IR（`contracts/01` §3.1、`设计教训.md` 反模式 2）。
// 决策表在编译期展开成等价规则列表，运行时不存在「表」这个概念。
//
// # 本包不做的事
//
// 不求值（B2/引擎侧）、不取数（A-2）、不管发布与审批（B8/B9）。
// 校验只做**静态**判断：字段存在性、类型、枚举、可空、白名单算子/函数、资源上限。
package dsl

import (
	"encoding/json"
	"fmt"
	"sort"
)

// 校验诊断错误码（`contracts/01` §3.7 与附录 A，本包补充见附录 A）。
const (
	CodeFieldNotFound       = "DSL_FIELD_NOT_FOUND"
	CodeTypeMismatch        = "DSL_TYPE_MISMATCH"
	CodeEnumInvalid         = "DSL_ENUM_INVALID"
	CodeOpNotAllowed        = "DSL_OP_NOT_ALLOWED"
	CodeSyntaxError         = "DSL_SYNTAX_ERROR"
	CodeFnNotFound          = "DSL_FN_NOT_FOUND"
	CodeDeprecatedField     = "DSL_DEPRECATED_FIELD"
	CodeNullableUnguarded   = "DSL_NULLABLE_UNGUARDED"
	CodeLimitExceeded       = "DSL_LIMIT_EXCEEDED"
	CodeOutputNotRegistered = "DSL_OUTPUT_NOT_REGISTERED"
	CodeDecisionNotReg      = "DSL_DECISION_NOT_REGISTERED"
	CodeReasonCodeInvalid   = "DSL_REASON_CODE_INVALID"
	CodeDuplicateRuleID     = "DSL_DUPLICATE_RULE_ID"
	CodeRuleSetInvalid      = "DSL_RULESET_INVALID"
	CodeTableInvalid        = "DSL_TABLE_INVALID"
	CodeTestInvalid         = "DSL_TEST_INVALID"
	CodeFactNotFound        = "DSL_FIELD_NOT_FOUND"
)

// Diagnostic 一条诊断：错误码 + 字段路径 + 建议。
//
// 路径形如 `rules[2].when.args[1].args[0]`，用**编译前的 DSL 形态**定位（不是 IR 形态），
// 这样规则作者能直接对着自己写的东西改。诊断必须让 agent 能自我纠错（`contracts/01` §3.7）。
type Diagnostic struct {
	Code    string `json:"code"`
	Level   string `json:"level"` // error | warn
	Path    string `json:"path"`
	RuleID  string `json:"ruleId,omitempty"`
	Fact    string `json:"fact,omitempty"`
	Message string `json:"message"`
	Suggest string `json:"suggest,omitempty"`
}

// diag 收集诊断，避免每个分支都写一遍结构体。
type diag struct {
	list []Diagnostic
}

func (d *diag) add(level, code, path, ruleID, fact, msg string, suggest ...string) {
	s := ""
	if len(suggest) > 0 {
		s = suggest[0]
	}
	d.list = append(d.list, Diagnostic{
		Code: code, Level: level, Path: path, RuleID: ruleID, Fact: fact,
		Message: msg, Suggest: s,
	})
}

func (d *diag) errorf(code, path, ruleID, fact, msg string, suggest ...string) {
	d.add("error", code, path, ruleID, fact, msg, suggest...)
}

func (d *diag) warnf(code, path, ruleID, fact, msg string, suggest ...string) {
	d.add("warn", code, path, ruleID, fact, msg, suggest...)
}

func (d *diag) hasError() bool {
	for _, x := range d.list {
		if x.Level == "error" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

// Kind 表达式节点类型，与 `contracts/01` §4.2 的四形态一一对应。
type Kind int

const (
	KindLiteral Kind = iota
	KindFact
	KindSlot
	KindOp
)

// Literal DSL 字面量：`{ "type": "decimal", "value": "1000" }`。
type Literal struct {
	Kind  string // integer / decimal / string / boolean / enum / array / null
	Value any
}

// Expr 表达式 AST。四种形态共用一张结构：字段按 Kind 生效。
type Expr struct {
	Kind    Kind
	Lit     *Literal
	Fact    string
	Slot    int
	Op      string
	Args    []*Expr
	Negated bool
	Func    string
}

// IsCall 判断是否为白名单函数调用（`{"op":"call","func":"..."}`）。
func (e *Expr) IsCall() bool { return e != nil && e.Kind == KindOp && e.Op == "call" }

// Depth 表达式深度（叶子 = 1）。
func (e *Expr) Depth() int {
	if e == nil {
		return 0
	}
	if e.Kind != KindOp {
		return 1
	}
	max := 0
	for _, a := range e.Args {
		if d := a.Depth(); d > max {
			max = d
		}
	}
	return max + 1
}

// NodeCount 节点数。
func (e *Expr) NodeCount() int {
	if e == nil {
		return 0
	}
	if e.Kind != KindOp {
		return 1
	}
	n := 1
	for _, a := range e.Args {
		n += a.NodeCount()
	}
	return n
}

// Walk 先序遍历。
func (e *Expr) Walk(fn func(*Expr)) {
	if e == nil {
		return
	}
	fn(e)
	for _, a := range e.Args {
		a.Walk(fn)
	}
}

// Facts 收集引用的字段（出现顺序、去重）。
func (e *Expr) Facts() []string {
	var out []string
	seen := map[string]bool{}
	e.Walk(func(n *Expr) {
		if n.Kind == KindFact && !seen[n.Fact] {
			seen[n.Fact] = true
			out = append(out, n.Fact)
		}
	})
	return out
}

// JSON 转回契约形态（`{"op":..,"args":[..]}` / `{"type":..,"value":..}` / `{"fact":..}` / `{"slot":..}`）。
//
// 用 map 而不是结构体，是为了让编码器按 key 升序输出，得到**字节稳定**的产物
// （`contracts/01` §5.1 编译确定性）。
func (e *Expr) JSON() any {
	switch e.Kind {
	case KindFact:
		return map[string]any{"fact": e.Fact}
	case KindSlot:
		return map[string]any{"slot": e.Slot}
	case KindLiteral:
		return map[string]any{"type": e.Lit.Kind, "value": e.Lit.Value}
	case KindOp:
		obj := map[string]any{"op": e.Op}
		args := make([]any, 0, len(e.Args))
		for _, a := range e.Args {
			args = append(args, a.JSON())
		}
		obj["args"] = args
		if e.Negated {
			obj["negated"] = true
		}
		if e.Op == "call" {
			obj["func"] = e.Func
		}
		return obj
	}
	return nil
}

// MarshalJSON 让 AST 直接以契约形态序列化。
func (e *Expr) MarshalJSON() ([]byte, error) { return json.Marshal(e.JSON()) }

// Equal 结构相等（用于测试与差异分析）。
func (e *Expr) Equal(o *Expr) bool {
	if e == nil || o == nil {
		return e == o
	}
	a, _ := json.Marshal(e.JSON())
	b, _ := json.Marshal(o.JSON())
	return string(a) == string(b)
}

// sortFacts 排序去重后的字段列表（生成物确定性）。
func sortFacts(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

// describe 用于诊断里说明「你写的是什么」。
func (e *Expr) describe() string {
	if e == nil {
		return "空表达式"
	}
	switch e.Kind {
	case KindFact:
		return "字段 " + e.Fact
	case KindSlot:
		return fmt.Sprintf("slot %d", e.Slot)
	case KindLiteral:
		return fmt.Sprintf("字面量(%s)", e.Lit.Kind)
	default:
		if e.IsCall() {
			return "函数 " + e.Func
		}
		return "运算符 " + e.Op
	}
}
