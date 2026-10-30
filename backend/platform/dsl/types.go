package dsl

import (
	"encoding/json"
	"fmt"
	"regexp"

	"goldenrules/platform/contracts"
)

// Definition 是平台侧的规则集定义（DSL 形态，未经编译）。
//
// 一条硬约定（`contracts/01` §3.1）：**规则集是模块单位，表达式/决策表只是它的写法**。
// 所以 `Rules` 与 `Table` **互斥**——同一个规则集只能选一种写法，编译后都变成同一份 IR。
type Definition struct {
	RulesetID string `json:"rulesetId"`
	Name      string `json:"name,omitempty"`
	Scene     string `json:"scene"`
	Version   string `json:"version,omitempty"`

	HitStrategy     string `json:"hitStrategy,omitempty"` // ALL（默认）/ FIRST / PRIORITY
	DefaultDecision string `json:"defaultDecision"`

	OutputSchemaID      string `json:"outputSchemaId,omitempty"`
	OutputSchemaVersion string `json:"outputSchemaVersion,omitempty"`

	// 写法一：表达式规则（when 可为结构化 JSON 或文本表达式糖）
	Rules []contracts.Rule `json:"rules,omitempty"`

	// 写法二：决策表（编译期展开成等价规则列表）
	Table *Table `json:"table,omitempty"`

	// 场景输出 schema。P0 随规则集携带（还没有注册中心），发布时并入规则包；
	// P1 迁到注册中心后这里只留 id/version（`contracts/03` §5）。
	OutputSchema *contracts.OutputSchema `json:"outputSchema,omitempty"`

	// 测试用例随规则集存放，发布时并入规则包（`contracts/01` §6.1）
	Tests []contracts.TestCase `json:"tests,omitempty"`

	UpdatedAt string `json:"updatedAt,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
	Revision  int64  `json:"revision,omitempty"`
}

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*$`)
var outputVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Table 决策表（`contracts/01` §3.2.4）。
//
// 行 = 一条规则（各列条件取 and），行序即优先级序；`*` 表示该维度通配。
// 它不是运行时模块，编译期就展开成表达式规则（`设计教训.md` 反模式 2）。
type Table struct {
	TableID    string          `json:"tableId"`
	Columns    []TableColumn   `json:"columns"`
	Rows       []TableRow      `json:"rows"`
	DefaultRow *contracts.Then `json:"defaultRow,omitempty"`
}

// TableColumn 条件维度：一列一个事实字段 + 类型（类型用于给单元格字面量定类型）。
type TableColumn struct {
	Fact string `json:"fact"`
	Type string `json:"type,omitempty"`
}

// TableRow 一行：conditions 的 key 是列事实名。
type TableRow struct {
	Conditions map[string]json.RawMessage `json:"conditions"`
	Then       contracts.Then             `json:"then"`
}

// ExpandTable 把决策表展开成等价规则列表（`contracts/01` §3.2.4 / §7.5）。
//
// 展开规则固定且可复现：
//   - 行 i → 规则 ID `<tableId>-R<i+1>`；
//   - 优先级 = `(行数 - i) * 10`，保证行序即优先级序且留出插行空间；
//   - `*` 列的单元格不产生条件；字面量单元格 → `eq`；`{op,value}` → 对应比较；
//   - 所有条件取 `and`，单条件时不套 `and`（IR 更短，语义相同）。
func ExpandTable(t *Table, d *diag) []contracts.Rule {
	if t == nil {
		return nil
	}
	if t.TableID == "" {
		d.errorf(CodeTableInvalid, "table.tableId", "", "", "决策表缺少 tableId")
	}
	if len(t.Columns) == 0 {
		d.errorf(CodeTableInvalid, "table.columns", "", "", "决策表至少需要一个条件列")
	}
	if len(t.Rows) == 0 {
		d.errorf(CodeTableInvalid, "table.rows", "", "", "决策表至少需要一行")
	}

	colType := map[string]string{}
	for i, c := range t.Columns {
		if c.Fact == "" {
			d.errorf(CodeTableInvalid, fmt.Sprintf("table.columns[%d].fact", i), "", "", "条件列缺少 fact")
			continue
		}
		if _, dup := colType[c.Fact]; dup {
			d.errorf(CodeTableInvalid, fmt.Sprintf("table.columns[%d].fact", i), "", c.Fact,
				"条件列重复："+c.Fact)
		}
		if c.Type == "" {
			d.warnf(CodeTableInvalid, fmt.Sprintf("table.columns[%d].type", i), "", c.Fact,
				"条件列未声明 type，单元格字面量按字符串处理", "建议声明 integer/decimal/enum 等类型")
		}
		colType[c.Fact] = c.Type
	}

	var rules []contracts.Rule
	for i, row := range t.Rows {
		ruleID := fmt.Sprintf("%s-R%d", t.TableID, i+1)
		args := make([]*Expr, 0, len(t.Columns))
		for _, col := range t.Columns {
			cell, ok := row.Conditions[col.Fact]
			if !ok {
				d.errorf(CodeTableInvalid, fmt.Sprintf("table.rows[%d].conditions", i), ruleID, "",
					"第 "+fmt.Sprint(i+1)+" 行缺少列 "+col.Fact+" 的条件",
					"通配写 \"*\"，等值写字面量，比较写 {\"op\":\"gt\",\"value\":..}")
				continue
			}
			if isWildcard(cell) {
				continue
			}
			e := cellExpr(col, cell, fmt.Sprintf("table.rows[%d].conditions[%s]", i, col.Fact), ruleID, d)
			if e != nil {
				args = append(args, e)
			}
		}
		for fact := range row.Conditions {
			if _, ok := colType[fact]; !ok {
				d.errorf(CodeTableInvalid, fmt.Sprintf("table.rows[%d].conditions", i), ruleID, fact,
					"条件里出现了未声明的列 "+fact, "先在 columns 里登记该事实")
			}
		}

		when := &Expr{Kind: KindOp, Op: "and", Args: args}
		if len(args) == 1 {
			when = args[0]
		}
		if len(args) == 0 {
			// 全通配行 = 永远命中，保留为显式 true，避免空表达式
			when = &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "boolean", Value: true}}
		}
		rules = append(rules, contracts.Rule{
			RuleID:   ruleID,
			Priority: int64(len(t.Rows)-i) * 10,
			When:     mustJSON(when),
			Then:     row.Then,
		})
	}
	return rules
}

func isWildcard(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "*"
}

// cellExpr 把一个单元格转成条件表达式。
func cellExpr(col TableColumn, raw json.RawMessage, path, ruleID string, d *diag) *Expr {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if opv, ok := obj["op"].(string); ok {
			val, hasVal := obj["value"]
			if !hasVal {
				d.errorf(CodeTableInvalid, path, ruleID, col.Fact,
					"单元格比较缺少 value", `写成 {"op":"gt","value":"1000"}`)
				return nil
			}
			if !IsAllowedOp(opv) {
				d.errorf(CodeOpNotAllowed, path, ruleID, col.Fact,
					fmt.Sprintf("单元格运算符 %q 不在白名单内", opv),
					"白名单："+fmt.Sprint(AllowedOps()))
				return nil
			}
			lit, err := coerceLiteral(literalKindFor(col.Type), val)
			if err != nil {
				d.errorf(CodeTypeMismatch, path+".value", ruleID, col.Fact, err.Error())
				return nil
			}
			return &Expr{Kind: KindOp, Op: opv, Args: []*Expr{
				{Kind: KindFact, Fact: col.Fact},
				{Kind: KindLiteral, Lit: lit},
			}}
		}
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		d.errorf(CodeTableInvalid, path, ruleID, col.Fact, "单元格无法解析")
		return nil
	}
	lit, err := coerceLiteral(literalKindFor(col.Type), v)
	if err != nil {
		d.errorf(CodeTypeMismatch, path, ruleID, col.Fact, err.Error())
		return nil
	}
	return &Expr{Kind: KindOp, Op: "eq", Args: []*Expr{
		{Kind: KindFact, Fact: col.Fact},
		{Kind: KindLiteral, Lit: lit},
	}}
}

// literalKindFor 列类型 → 单元格字面量类型。未声明类型时按字符串（最保守，不猜数字）。
func literalKindFor(colType string) string {
	switch colType {
	case "integer":
		return "integer"
	case "decimal":
		return "decimal"
	case "boolean":
		return "boolean"
	case "enum", "string":
		return "string"
	default:
		return "string"
	}
}

func mustJSON(e *Expr) json.RawMessage {
	body, err := json.Marshal(e.JSON())
	if err != nil {
		panic("dsl: 表达式序列化失败: " + err.Error())
	}
	return body
}
