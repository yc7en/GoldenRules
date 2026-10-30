package dsl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Allow-listed operators and functions (`contracts/01` §3.2.1 / §3.6 / Appendix B).
//
// The allow-list is the **only** legality criterion: anything not listed here is rejected with `DSL_OP_NOT_ALLOWED` /
// `DSL_FN_NOT_FOUND`, so "an unknown operator slipping through" is physically impossible (A-7).

type opSpec struct {
	minArgs int
	maxArgs int // -1 = unlimited
}

var opSpecs = map[string]opSpec{
	"and":    {1, -1},
	"or":     {1, -1},
	"not":    {1, 1},
	"eq":     {2, 2},
	"ne":     {2, 2},
	"gt":     {2, 2},
	"gte":    {2, 2},
	"lt":     {2, 2},
	"lte":    {2, 2},
	"add":    {2, 2},
	"sub":    {2, 2},
	"mul":    {2, 2},
	"div":    {2, 2},
	"mod":    {2, 2},
	"in":     {2, 2},
	"notIn":  {2, 2},
	"isNull": {1, 1},
	"call":   {0, -1},
}

var funcSpecs = map[string]opSpec{
	"now":       {0, 0},
	"inSet":     {2, 2},
	"abs":       {1, 1},
	"min":       {1, -1},
	"max":       {1, -1},
	"round":     {1, 2},
	"contains":  {2, 2},
	"stringLen": {1, 1},
	"toUpper":   {1, 1},
	"toLower":   {1, 1},
	"substr":    {2, 3},
	"isNull":    {1, 1},
	"coalesce":  {1, -1},
}

// IsAllowedOp reports whether the operator is on the allow-list.
func IsAllowedOp(op string) bool {
	_, ok := opSpecs[op]
	return ok
}

// OpArity returns the allowed argument-count range of an operator.
func OpArity(op string) (int, int) {
	s := opSpecs[op]
	return s.minArgs, s.maxArgs
}

// IsAllowedFunc reports whether the function is on the allow-list.
func IsAllowedFunc(fn string) bool {
	_, ok := funcSpecs[fn]
	return ok
}

// FuncArity returns the allowed argument-count range of a function.
func FuncArity(fn string) (int, int) {
	s := funcSpecs[fn]
	return s.minArgs, s.maxArgs
}

// AllowedOps lists the allow-listed operators (sorted, used in diagnostic suggestions).
func AllowedOps() []string {
	out := make([]string, 0, len(opSpecs))
	for k := range opSpecs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AllowedFuncs lists the allow-listed functions (sorted).
func AllowedFuncs() []string {
	out := make([]string, 0, len(funcSpecs))
	for k := range funcSpecs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Text expression (syntactic sugar) lexer
// ---------------------------------------------------------------------------

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokNumber
	tokString
	tokPunct
)

type token struct {
	kind tokKind
	val  string
	pos  int
}

func lex(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			i++
			var sb strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					switch s[i+1] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					default:
						sb.WriteByte(s[i+1])
					}
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					closed = true
					break
				}
				sb.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("字符串未闭合（位置 %d）", i)
			}
			out = append(out, token{tokString, sb.String(), i})
		case c >= '0' && c <= '9':
			start := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if i < len(s) && s[i] == '.' {
				i++
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
			}
			out = append(out, token{tokNumber, s[start:i], start})
		case isIdentStart(c):
			start := i
			for i < len(s) {
				if isIdentPart(s[i]) {
					i++
					continue
				}
				// namespace separator: `request.amount` (the dot must be followed by an identifier-start character)
				if s[i] == '.' && i+1 < len(s) && isIdentStart(s[i+1]) {
					i++
					continue
				}
				break
			}
			out = append(out, token{tokIdent, s[start:i], start})
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "&&", "||", "==", "!=", ">=", "<=":
				out = append(out, token{tokPunct, two, i})
				i += 2
				continue
			}
			switch c {
			case '!', '>', '<', '+', '-', '*', '/', '%', '(', ')', '[', ']', ',':
				out = append(out, token{tokPunct, string(c), i})
				i++
				continue
			}
			return nil, fmt.Errorf("无法识别的字符 %q（位置 %d）", string(c), i)
		}
	}
	out = append(out, token{tokEOF, "", len(s)})
	return out, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// ---------------------------------------------------------------------------
// Text expression grammar (recursive descent)
// ---------------------------------------------------------------------------

type textParser struct {
	toks []token
	i    int
}

// parseText parses textual expression sugar into an AST.
//
// The expansion is fixed and reproducible (`contracts/01` §5.3): the same text always yields the same AST,
// hence the same IR; no implicit type conversion is performed.
func parseText(src string) (*Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &textParser{toks: toks}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("多余的内容 %q（位置 %d）", p.peek().val, p.peek().pos)
	}
	return e, nil
}

func (p *textParser) peek() token { return p.toks[p.i] }

func (p *textParser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *textParser) accept(val string) bool {
	if p.peek().kind == tokPunct && p.peek().val == val {
		p.i++
		return true
	}
	return false
}

func (p *textParser) isKeyword(word string) bool {
	t := p.peek()
	return t.kind == tokIdent && t.val == word
}

func (p *textParser) parseOr() (*Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	args := []*Expr{left}
	for p.accept("||") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		args = append(args, right)
	}
	if len(args) == 1 {
		return left, nil
	}
	return &Expr{Kind: KindOp, Op: "or", Args: args}, nil
}

func (p *textParser) parseAnd() (*Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	args := []*Expr{left}
	for p.accept("&&") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		args = append(args, right)
	}
	if len(args) == 1 {
		return left, nil
	}
	return &Expr{Kind: KindOp, Op: "and", Args: args}, nil
}

func (p *textParser) parseNot() (*Expr, error) {
	if p.accept("!") {
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &Expr{Kind: KindOp, Op: "not", Args: []*Expr{inner}}, nil
	}
	return p.parseCmp()
}

func (p *textParser) parseCmp() (*Expr, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}

	// is null / is not null
	if p.isKeyword("is") {
		p.next()
		negated := false
		if p.isKeyword("not") {
			p.next()
			negated = true
		}
		if !p.isKeyword("null") {
			return nil, fmt.Errorf("is 后只支持 null（位置 %d）", p.peek().pos)
		}
		p.next()
		return &Expr{Kind: KindOp, Op: "isNull", Args: []*Expr{left}, Negated: negated}, nil
	}

	// in / not in
	if p.isKeyword("not") {
		save := p.i
		p.next()
		if p.isKeyword("in") {
			p.next()
			listExpr, err := p.parseList()
			if err != nil {
				return nil, err
			}
			return &Expr{Kind: KindOp, Op: "notIn", Args: []*Expr{left, listExpr}}, nil
		}
		p.i = save
	}
	if p.isKeyword("in") {
		p.next()
		listExpr, err := p.parseList()
		if err != nil {
			return nil, err
		}
		return &Expr{Kind: KindOp, Op: "in", Args: []*Expr{left, listExpr}}, nil
	}

	ops := map[string]string{"==": "eq", "!=": "ne", ">": "gt", ">=": "gte", "<": "lt", "<=": "lte"}
	if t := p.peek(); t.kind == tokPunct {
		if op, ok := ops[t.val]; ok {
			p.next()
			right, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			// Reject chained comparisons (a > b > c), otherwise the semantics are ambiguous
			if t2 := p.peek(); t2.kind == tokPunct {
				if _, again := ops[t2.val]; again {
					return nil, fmt.Errorf("不支持连续比较（位置 %d），请用 && 显式连接", t2.pos)
				}
			}
			return &Expr{Kind: KindOp, Op: op, Args: []*Expr{left, right}}, nil
		}
	}
	return left, nil
}

func (p *textParser) parseAdd() (*Expr, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokPunct || (t.val != "+" && t.val != "-") {
			return left, nil
		}
		p.next()
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		op := "add"
		if t.val == "-" {
			op = "sub"
		}
		left = &Expr{Kind: KindOp, Op: op, Args: []*Expr{left, right}}
	}
}

func (p *textParser) parseMul() (*Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokPunct || (t.val != "*" && t.val != "/" && t.val != "%") {
			return left, nil
		}
		p.next()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		op := map[string]string{"*": "mul", "/": "div", "%": "mod"}[t.val]
		left = &Expr{Kind: KindOp, Op: op, Args: []*Expr{left, right}}
	}
}

func (p *textParser) parseUnary() (*Expr, error) {
	if p.accept("-") {
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		// Only literals may be negated, so no "unary minus" node outside the allow-list is introduced
		if inner.Kind == KindLiteral && inner.Lit.Kind == "integer" {
			if i, ok := inner.Lit.Value.(int64); ok {
				return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "integer", Value: -i}}, nil
			}
		}
		if inner.Kind == KindLiteral && inner.Lit.Kind == "decimal" {
			if s, ok := inner.Lit.Value.(string); ok {
				return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "decimal", Value: "-" + s}}, nil
			}
		}
		return nil, fmt.Errorf("负号只支持字面量（位置 %d）", p.peek().pos)
	}
	return p.parsePrimary()
}

func (p *textParser) parsePrimary() (*Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokNumber:
		p.next()
		n := numberLiteral(jsonNumber(t.val))
		return n, nil
	case tokString:
		p.next()
		return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "string", Value: t.val}}, nil
	case tokPunct:
		if t.val == "(" {
			p.next()
			inner, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if !p.accept(")") {
				return nil, fmt.Errorf("缺少右括号（位置 %d）", p.peek().pos)
			}
			return inner, nil
		}
		if t.val == "[" {
			return p.parseList()
		}
		return nil, fmt.Errorf("意外的符号 %q（位置 %d）", t.val, t.pos)
	case tokIdent:
		switch t.val {
		case "true", "false":
			p.next()
			return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "boolean", Value: t.val == "true"}}, nil
		case "null":
			p.next()
			return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "null", Value: nil}}, nil
		}
		p.next()
		// function call
		if p.peek().kind == tokPunct && p.peek().val == "(" {
			p.next()
			var args []*Expr
			if !(p.peek().kind == tokPunct && p.peek().val == ")") {
				for {
					a, err := p.parseOr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.accept(",") {
						continue
					}
					break
				}
			}
			if !p.accept(")") {
				return nil, fmt.Errorf("函数 %s 缺少右括号（位置 %d）", t.val, p.peek().pos)
			}
			if !IsAllowedFunc(t.val) {
				return nil, fmt.Errorf("函数 %s 不在白名单内", t.val)
			}
			return &Expr{Kind: KindOp, Op: "call", Func: t.val, Args: args}, nil
		}
		return &Expr{Kind: KindFact, Fact: t.val}, nil
	}
	return nil, fmt.Errorf("表达式不完整（位置 %d）", t.pos)
}

func (p *textParser) parseList() (*Expr, error) {
	if !p.accept("[") {
		return nil, fmt.Errorf("in 右侧必须是字面量列表（位置 %d）", p.peek().pos)
	}
	var items []any
	if !(p.peek().kind == tokPunct && p.peek().val == "]") {
		for {
			t := p.peek()
			var v any
			switch t.kind {
			case tokString:
				p.next()
				v = t.val
			case tokNumber:
				p.next()
				if strings.ContainsAny(t.val, ".") {
					v = t.val // decimals land as strings
				} else {
					i, err := strconv.ParseInt(t.val, 10, 64)
					if err != nil {
						return nil, fmt.Errorf("数字 %q 非法", t.val)
					}
					v = i
				}
			case tokIdent:
				if t.val == "true" || t.val == "false" {
					p.next()
					v = t.val == "true"
				} else if t.val == "null" {
					p.next()
					v = nil
				} else {
					return nil, fmt.Errorf("列表元素只支持字面量（位置 %d）", t.pos)
				}
			default:
				return nil, fmt.Errorf("列表元素只支持字面量（位置 %d）", t.pos)
			}
			items = append(items, v)
			if p.accept(",") {
				continue
			}
			break
		}
	}
	if !p.accept("]") {
		return nil, fmt.Errorf("列表缺少右方括号（位置 %d）", p.peek().pos)
	}
	return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "array", Value: items}}, nil
}
