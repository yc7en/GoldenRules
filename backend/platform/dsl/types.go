package dsl

import (
	"encoding/json"
	"fmt"
	"regexp"

	"goldenrules/platform/contracts"
)

// Definition is the platform-side rule set definition (DSL form, not yet compiled).
//
// One hard rule (`contracts/01` §3.1): **a rule set is the unit of modularity; expressions/decision tables are merely ways to write it**.
// Therefore `Rules` and `Table` are **mutually exclusive** — a rule set picks exactly one style, and both compile into the same IR.
type Definition struct {
	RulesetID string `json:"rulesetId"`
	Name      string `json:"name,omitempty"`
	Scene     string `json:"scene"`
	Version   string `json:"version,omitempty"`

	HitStrategy     string `json:"hitStrategy,omitempty"` // ALL (default) / FIRST / PRIORITY
	DefaultDecision string `json:"defaultDecision"`

	OutputSchemaID      string `json:"outputSchemaId,omitempty"`
	OutputSchemaVersion string `json:"outputSchemaVersion,omitempty"`

	// Style 1: expression rules (when may be structured JSON or textual expression sugar)
	Rules []contracts.Rule `json:"rules,omitempty"`

	// Style 2: decision table (expanded into an equivalent rule list at compile time)
	Table *Table `json:"table,omitempty"`

	// Scene output schema. In P0 it travels with the rule set (there is no registry yet) and is merged into the rule package on publish;
	// once it migrates to the registry in P1, only id/version stay here (`contracts/03` §5).
	OutputSchema *contracts.OutputSchema `json:"outputSchema,omitempty"`

	// Test cases are stored alongside the rule set and merged into the rule package on publish (`contracts/01` §6.1)
	Tests []contracts.TestCase `json:"tests,omitempty"`

	UpdatedAt string `json:"updatedAt,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
	Revision  int64  `json:"revision,omitempty"`
}

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*$`)
var outputVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Table is a decision table (`contracts/01` §3.2.4).
//
// A row = one rule (the column conditions are and-ed together), row order is priority order; `*` means that dimension is a wildcard.
// It is not a runtime module: it is expanded into expression rules at compile time (`设计教训.md` anti-pattern 2).
type Table struct {
	TableID    string          `json:"tableId"`
	Columns    []TableColumn   `json:"columns"`
	Rows       []TableRow      `json:"rows"`
	DefaultRow *contracts.Then `json:"defaultRow,omitempty"`
}

// TableColumn is a condition dimension: one fact field + type per column (the type types the cell literals).
type TableColumn struct {
	Fact string `json:"fact"`
	Type string `json:"type,omitempty"`
}

// TableRow is a single row: the keys of conditions are the column fact names.
type TableRow struct {
	Conditions map[string]json.RawMessage `json:"conditions"`
	Then       contracts.Then             `json:"then"`
}

// ExpandTable expands a decision table into an equivalent rule list (`contracts/01` §3.2.4 / §7.5).
//
// The expansion is fixed and reproducible:
//   - row i → rule ID `<tableId>-R<i+1>`;
//   - priority = `(row count - i) * 10`, so row order is priority order and there is room to insert rows;
//   - cells in a `*` column produce no condition; a literal cell → `eq`; `{op,value}` → the corresponding comparison;
//   - all conditions are and-ed; a single condition is not wrapped in `and` (shorter IR, same semantics).
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
			// an all-wildcard row = always matched; keep it as an explicit true to avoid an empty expression
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

// cellExpr converts one cell into a condition expression.
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

// literalKindFor maps a column type → cell literal kind. Without a declared type it assumes string (most conservative, no guessing numbers).
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
