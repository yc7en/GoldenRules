package dsl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ParseWhen 解析 `when` 表达式。**字符串形态按文本表达式糖处理**（`contracts/01` §3.2.1）：
//
//	"when": "request.amount > 1000 && request.currency == \"CNY\""
//
// 只想要一个纯字面量时，写成 `{"type":"boolean","value":true}`。
func ParseWhen(raw json.RawMessage, path string, d *diag) *Expr {
	return parseExpr(raw, path, true, d)
}

// ParseValue 解析 `assign[].expr`。字符串形态按**字面量**处理（`contracts/01` §3.2.3 的
// `{"var":"riskLevel","expr":"HIGH"}`），要写表达式请用结构化形态。
func ParseValue(raw json.RawMessage, path string, d *diag) *Expr {
	return parseExpr(raw, path, false, d)
}

func parseExpr(raw json.RawMessage, path string, textSugar bool, d *diag) *Expr {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		d.errorf(CodeSyntaxError, path, "", "", "表达式为空", "when / expr 必须是表达式，不能省略")
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		d.errorf(CodeSyntaxError, path, "", "", "表达式不是合法 JSON: "+err.Error())
		return nil
	}
	return parseValue(v, path, textSugar, d)
}

func parseValue(v any, path string, textSugar bool, d *diag) *Expr {
	switch t := v.(type) {
	case string:
		if textSugar {
			e, err := parseText(t)
			if err != nil {
				d.errorf(CodeSyntaxError, path, "", "", "文本表达式解析失败: "+err.Error(),
					"检查括号是否配对、字符串是否用双引号、运算符是否在白名单内")
				return nil
			}
			return e
		}
		return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "string", Value: t}}
	case bool:
		return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "boolean", Value: t}}
	case nil:
		return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "null", Value: nil}}
	case json.Number:
		return numberLiteral(t)
	case float64:
		return numberLiteral(json.Number(strconv.FormatFloat(t, 'f', -1, 64)))
	case []any:
		items := make([]any, 0, len(t))
		for i, item := range t {
			e := parseValue(item, fmt.Sprintf("%s[%d]", path, i), false, d)
			if e == nil {
				return nil
			}
			if e.Kind != KindLiteral {
				d.errorf(CodeTypeMismatch, fmt.Sprintf("%s[%d]", path, i), "", "",
					"数组中只允许字面量", "数组用于 inSet / in 的候选集合")
				return nil
			}
			items = append(items, e.Lit.Value)
		}
		return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "array", Value: items}}
	case map[string]any:
		return parseObject(t, path, d)
	}
	d.errorf(CodeSyntaxError, path, "", "", fmt.Sprintf("无法识别的表达式节点: %T", v))
	return nil
}

// numberLiteral 无小数点的按 integer，其余按 decimal（值以字符串落地，避免浮点，A-5）。
func numberLiteral(n json.Number) *Expr {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := n.Int64(); err == nil {
			return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "integer", Value: i}}
		}
	}
	return &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "decimal", Value: s}}
}

func parseObject(m map[string]any, path string, d *diag) *Expr {
	// 字面量：{"type": "...", "value": ...}
	if tv, ok := m["type"]; ok {
		kind, _ := tv.(string)
		val, hasVal := m["value"]
		if !hasVal {
			d.errorf(CodeSyntaxError, path, "", "", "字面量缺少 value 字段")
			return nil
		}
		if kind == "" {
			d.errorf(CodeSyntaxError, path, "", "", "字面量缺少 type 字段")
			return nil
		}
		lit, err := coerceLiteral(kind, val)
		if err != nil {
			d.errorf(CodeTypeMismatch, path+".value", "", "", err.Error())
			return nil
		}
		return &Expr{Kind: KindLiteral, Lit: lit}
	}

	// 字段引用：{"fact": "namespace.field"}
	if fv, ok := m["fact"]; ok {
		name, _ := fv.(string)
		if strings.TrimSpace(name) == "" {
			d.errorf(CodeSyntaxError, path, "", "", "字段引用缺少字段名")
			return nil
		}
		return &Expr{Kind: KindFact, Fact: name}
	}

	// slot 引用：编译后形态，dry-run 阶段出现说明作者误用了 IR
	if sv, ok := m["slot"]; ok {
		if n, ok := sv.(json.Number); ok {
			i, _ := n.Int64()
			return &Expr{Kind: KindSlot, Slot: int(i)}
		}
		d.errorf(CodeSyntaxError, path, "", "", "slot 必须是整数")
		return nil
	}

	// 运算：{"op": "...", "args": [...]}
	opv, ok := m["op"]
	if !ok {
		d.errorf(CodeSyntaxError, path, "", "",
			"表达式对象必须有 op / fact / type 之一",
			`合法写法：{"op":"gt","args":[{"fact":"request.amount"},1000]}`)
		return nil
	}
	op, _ := opv.(string)
	if op == "" {
		d.errorf(CodeSyntaxError, path, "", "", "op 不能为空")
		return nil
	}
	e := &Expr{Kind: KindOp, Op: op}
	if neg, ok := m["negated"].(bool); ok {
		e.Negated = neg
	}
	if fn, ok := m["func"].(string); ok {
		e.Func = fn
	}

	// 白名单先判，避免为非法 op 继续递归出一堆噪声诊断。
	if !IsAllowedOp(op) {
		d.errorf(CodeOpNotAllowed, path+".op", "", "",
			fmt.Sprintf("运算符 %q 不在白名单内", op),
			"白名单："+strings.Join(AllowedOps(), " / "))
		return nil
	}

	args, _ := m["args"].([]any)
	if len(args) == 0 {
		d.errorf(CodeSyntaxError, path+".args", "", "", "运算符 "+op+" 缺少 args",
			"无参运算请改用函数调用 now()")
		return nil
	}
	for i, a := range args {
		child := parseValue(a, fmt.Sprintf("%s.args[%d]", path, i), false, d)
		if child == nil {
			return nil
		}
		e.Args = append(e.Args, child)
	}

	if op == "call" {
		if e.Func == "" {
			d.errorf(CodeSyntaxError, path+".func", "", "", "call 缺少 func 字段",
				"函数调用写成 {\"op\":\"call\",\"func\":\"abs\",\"args\":[..]}")
			return nil
		}
		if !IsAllowedFunc(e.Func) {
			d.errorf(CodeFnNotFound, path+".func", "", "",
				fmt.Sprintf("函数 %q 不在白名单内", e.Func),
				"白名单："+strings.Join(AllowedFuncs(), " / "))
			return nil
		}
	}
	return e
}

// coerceLiteral 按声明类型归一化字面量值。
func coerceLiteral(kind string, val any) (*Literal, error) {
	switch kind {
	case "integer":
		switch t := val.(type) {
		case json.Number:
			if i, err := t.Int64(); err == nil {
				return &Literal{Kind: "integer", Value: i}, nil
			}
			return nil, fmt.Errorf("integer 字面量不是整数: %v", t)
		case float64:
			if t == float64(int64(t)) {
				return &Literal{Kind: "integer", Value: int64(t)}, nil
			}
			return nil, fmt.Errorf("integer 字面量不是整数: %v", t)
		}
		return nil, fmt.Errorf("integer 字面量期望数字，得到 %T", val)
	case "decimal":
		switch t := val.(type) {
		case json.Number:
			return &Literal{Kind: "decimal", Value: t.String()}, nil
		case string:
			return &Literal{Kind: "decimal", Value: strings.TrimSpace(t)}, nil
		case float64:
			return &Literal{Kind: "decimal", Value: strconv.FormatFloat(t, 'f', -1, 64)}, nil
		}
		return nil, fmt.Errorf("decimal 字面量期望字符串或数字，得到 %T", val)
	case "string", "enum":
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("%s 字面量期望字符串，得到 %T", kind, val)
		}
		return &Literal{Kind: kind, Value: s}, nil
	case "boolean":
		b, ok := val.(bool)
		if !ok {
			return nil, fmt.Errorf("boolean 字面量期望 true/false，得到 %T", val)
		}
		return &Literal{Kind: "boolean", Value: b}, nil
	case "array":
		arr, ok := val.([]any)
		if !ok {
			return nil, fmt.Errorf("array 字面量期望数组，得到 %T", val)
		}
		return &Literal{Kind: "array", Value: arr}, nil
	case "null":
		return &Literal{Kind: "null", Value: nil}, nil
	}
	return nil, fmt.Errorf("未知字面量类型 %q（合法：integer/decimal/string/boolean/enum/array）", kind)
}

// jsonNumber 便于把词法阶段的数字文本转成 json.Number（保持精度，不经 float）。
func jsonNumber(s string) json.Number { return json.Number(s) }
