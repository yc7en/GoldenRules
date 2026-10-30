// Package dsl implements B7: parsing, validation and compilation (dry-run) of the rule DSL.
//
// Contract basis: `contracts/01` (DSL and IR specification), `contracts/05` §6 (validation pipeline).
//
// # Three authoring styles, one IR
//
// Structured JSON, textual expression sugar and decision tables are **expressively equivalent**;
// all three converge into this package's [Expr] AST first, then compile into the same IR (`contracts/01` §3.1,
// `设计教训.md` anti-pattern 2). Decision tables expand into an equivalent rule list at compile time; no "table" exists at runtime.
//
// # What this package does not do
//
// It does not evaluate (B2/engine side), does not fetch facts (A-2), and does not handle publishing or approval (B8/B9).
// Validation makes **static** judgements only: field existence, type, enum, nullability, allow-listed operators/functions, and resource limits.
package dsl

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Validation diagnostic error codes (`contracts/01` §3.7 and Appendix A; additions from this package are listed in Appendix A).
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

// Diagnostic is a single diagnostic: error code + field path + suggestion.
//
// The path looks like `rules[2].when.args[1].args[0]` and is anchored in the **pre-compilation DSL form** (not the IR form),
// so rule authors can fix exactly what they wrote. Diagnostics must let the agent self-correct (`contracts/01` §3.7).
type Diagnostic struct {
	Code    string `json:"code"`
	Level   string `json:"level"` // error | warn
	Path    string `json:"path"`
	RuleID  string `json:"ruleId,omitempty"`
	Fact    string `json:"fact,omitempty"`
	Message string `json:"message"`
	Suggest string `json:"suggest,omitempty"`
}

// diag collects diagnostics so that every branch does not have to repeat the struct literal.
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

// Kind is the expression node kind, one-to-one with the four forms in `contracts/01` §4.2.
type Kind int

const (
	KindLiteral Kind = iota
	KindFact
	KindSlot
	KindOp
)

// Literal is a DSL literal: `{ "type": "decimal", "value": "1000" }`.
type Literal struct {
	Kind  string // integer / decimal / string / boolean / enum / array / null
	Value any
}

// Expr is the expression AST. The four forms share one struct: fields apply according to Kind.
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

// IsCall reports whether this is an allow-listed function call (`{"op":"call","func":"..."}`).
func (e *Expr) IsCall() bool { return e != nil && e.Kind == KindOp && e.Op == "call" }

// Depth is the expression depth (a leaf = 1).
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

// NodeCount is the number of nodes.
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

// Walk performs a pre-order traversal.
func (e *Expr) Walk(fn func(*Expr)) {
	if e == nil {
		return
	}
	fn(e)
	for _, a := range e.Args {
		a.Walk(fn)
	}
}

// Facts collects the referenced fields (order of appearance, de-duplicated).
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

// JSON converts back to the contract form (`{"op":..,"args":[..]}` / `{"type":..,"value":..}` / `{"fact":..}` / `{"slot":..}`).
//
// A map instead of a struct is used so the encoder emits keys in ascending order and the output is
// **byte-stable** (`contracts/01` §5.1 compile determinism).
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

// MarshalJSON serialises the AST directly in its contract form.
func (e *Expr) MarshalJSON() ([]byte, error) { return json.Marshal(e.JSON()) }

// Equal reports structural equality (used by tests and diff analysis).
func (e *Expr) Equal(o *Expr) bool {
	if e == nil || o == nil {
		return e == o
	}
	a, _ := json.Marshal(e.JSON())
	b, _ := json.Marshal(o.JSON())
	return string(a) == string(b)
}

// sortFacts sorts the de-duplicated field list (artifact determinism).
func sortFacts(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

// describe explains "what you wrote" inside a diagnostic.
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
