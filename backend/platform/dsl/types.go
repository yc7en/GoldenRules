package dsl

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

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

	// Style 3: cross decision table / decision matrix (`contracts/01` §3.2.5), also expanded at compile time.
	// Mutually exclusive with both of the above: the three spellings store different shapes, and turning one
	// into another is lossy (a cross table remembers which axis each dimension came from; a flat table does not).
	CrossTable *CrossTable `json:"crossTable,omitempty"`

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
//
// There is deliberately no `defaultRow`: a table-level fallback would duplicate `Definition.DefaultDecision`,
// and when the two disagree there is no principled way to pick a winner. The rule set's `defaultDecision` is the only fallback.
type Table struct {
	TableID string        `json:"tableId"`
	Columns []TableColumn `json:"columns"`
	Rows    []TableRow    `json:"rows"`
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

// CrossTable is a cross decision table / decision matrix (`contracts/01` §3.2.5).
//
// Two axes of conditions pick out exactly one cell, and the cell's literal is assigned to a single target variable.
// The whole table shares one decision: that is the point of a matrix (fee tables, pricing grids, quota tiers) - the
// conclusion is uniform and only the parameter varies. When different cells need different conclusions, use `Table`.
//
// Like `Table` it is not a runtime module: `ExpandCrossTable` turns each non-empty cell into one ordinary rule.
type CrossTable struct {
	CrossTableID string `json:"crossTableId"`
	// RowHeaders / ColHeaders are the condition dimensions of each axis (fact + type).
	RowHeaders []TableColumn `json:"rowHeaders"`
	ColHeaders []TableColumn `json:"colHeaders"`
	// RowCells[i] / ColCells[j] hold the axis condition per header fact. The encoding is byte-identical to a
	// `Table` cell - a bare literal means equality, `{op,value}` means a comparison - so `cellExpr` is reused as is.
	RowCells []map[string]json.RawMessage `json:"rowCells"`
	ColCells []map[string]json.RawMessage `json:"colCells"`
	// Cells[i][j] is the value assigned when row i and column j both match; `null` means "no rule here".
	Cells  [][]json.RawMessage `json:"cells"`
	Target CrossTarget         `json:"target"`
	// Then carries the uniform conclusion. Its `assign` must be empty - values come from the cells.
	Then contracts.Then `json:"then"`
}

// CrossTarget is the variable a cell value is assigned to. Type is mandatory: without it there is no way to
// decide whether `"0.10"` is a decimal or a string.
type CrossTarget struct {
	Var  string `json:"var"`
	Type string `json:"type"`
}

// ExpandCrossTable expands a cross table into an equivalent rule list (`contracts/01` §3.2.5).
//
// The expansion is fixed and reproducible:
//   - traversal is row-major: top to bottom, then left to right;
//   - cell (i,j) → rule ID `<crossTableId>-R<i+1>C<j+1>`, priority `(total-seq+1) * 10` where seq counts every
//     cell including the empty ones, so priorities stay stable when a cell is cleared;
//   - when = and(row conditions..., column conditions...), no `and` wrapper for a single condition;
//   - then = the table's decision/reasonCode plus one assign of the cell literal to `target.var`;
//   - a `null`/absent cell produces no rule.
func ExpandCrossTable(t *CrossTable, d *diag) []contracts.Rule {
	if t == nil {
		return nil
	}
	if t.CrossTableID == "" {
		d.errorf(CodeTableInvalid, "crossTable.crossTableId", "", "", "交叉表缺少 crossTableId")
	}
	if len(t.RowHeaders) == 0 {
		d.errorf(CodeTableInvalid, "crossTable.rowHeaders", "", "", "交叉表至少需要一个纵向条件列")
	}
	if len(t.ColHeaders) == 0 {
		d.errorf(CodeTableInvalid, "crossTable.colHeaders", "", "", "交叉表至少需要一个横向条件列")
	}
	if len(t.RowCells) == 0 {
		d.errorf(CodeTableInvalid, "crossTable.rowCells", "", "", "交叉表至少需要一行")
	}
	if len(t.ColCells) == 0 {
		d.errorf(CodeTableInvalid, "crossTable.colCells", "", "", "交叉表至少需要一列")
	}
	if t.Target.Var == "" {
		d.errorf(CodeTableInvalid, "crossTable.target.var", "", "", "交叉表缺少赋值目标 target.var",
			"交叉表的动作只有一个：把格子里的值赋给某个输出变量")
	} else if !outputVarPattern.MatchString(t.Target.Var) {
		d.errorf(CodeTableInvalid, "crossTable.target.var", "", "",
			"变量名只能是字母/下划线开头的标识符："+t.Target.Var)
	}
	if t.Target.Type == "" {
		d.errorf(CodeTableInvalid, "crossTable.target.type", "", "",
			"交叉表缺少赋值目标类型 target.type",
			"类型决定格子字面量怎么解释：integer / decimal / boolean / string / enum")
	}
	if len(t.Then.Assign) > 0 {
		d.errorf(CodeTableInvalid, "crossTable.then.assign", "", "",
			"交叉表的单元格就是赋值来源，then.assign 必须为空",
			"把值填进 cells，或改用普通决策表")
	}

	// Facets may not repeat across or within an axis: `x=a and x=b` is unsatisfiable, so such a cell could never fire.
	axisOf := map[string]string{}
	for _, axis := range []struct {
		name    string
		headers []TableColumn
	}{{"rowHeaders", t.RowHeaders}, {"colHeaders", t.ColHeaders}} {
		for i, h := range axis.headers {
			path := fmt.Sprintf("crossTable.%s[%d].fact", axis.name, i)
			if h.Fact == "" {
				d.errorf(CodeTableInvalid, path, "", "", "条件列缺少 fact")
				continue
			}
			if prev, dup := axisOf[h.Fact]; dup {
				if prev == axis.name {
					d.errorf(CodeTableInvalid, path, "", h.Fact, "同一维度里条件列重复："+h.Fact)
				} else {
					d.errorf(CodeTableInvalid, path, "", h.Fact,
						"同一个事实不能同时作为纵向与横向条件："+h.Fact,
						"x=a 且 x=b 恒不成立，这一格永远不会命中")
				}
				continue
			}
			axisOf[h.Fact] = axis.name
			if h.Type == "" {
				d.warnf(CodeTableInvalid, path, "", h.Fact,
					"条件列未声明 type，格子字面量按字符串处理", "建议声明 integer/decimal/enum 等类型")
			}
		}
	}

	rowArgs := expandCrossAxis(t.RowCells, t.RowHeaders, "rowCells", "行", d)
	colArgs := expandCrossAxis(t.ColCells, t.ColHeaders, "colCells", "列", d)

	if len(t.Cells) != len(t.RowCells) {
		d.errorf(CodeTableInvalid, "crossTable.cells", "", "",
			fmt.Sprintf("cells 有 %d 行，与纵向条件 %d 行不一致", len(t.Cells), len(t.RowCells)),
			"格子与行列条件一一对应，形状不一致会让取值静默错位")
	}

	total := len(t.RowCells) * len(t.ColCells)
	rules := make([]contracts.Rule, 0, total)
	seq := 0
	for i := range t.RowCells {
		for j := range t.ColCells {
			seq++
			if i >= len(t.Cells) || j >= len(t.Cells[i]) {
				continue // shape mismatch already reported above
			}
			raw := t.Cells[i][j]
			if isEmptyCell(raw) {
				continue
			}
			ruleID := fmt.Sprintf("%s-R%dC%d", t.CrossTableID, i+1, j+1)
			path := fmt.Sprintf("crossTable.cells[%d][%d]", i, j)

			lit, err := crossCellLiteral(raw, t.Target.Type)
			if err != nil {
				d.errorf(CodeTypeMismatch, path, ruleID, t.Target.Var, err.Error())
				continue
			}

			args := make([]*Expr, 0, len(rowArgs[i])+len(colArgs[j]))
			args = append(args, rowArgs[i]...)
			args = append(args, colArgs[j]...)
			when := &Expr{Kind: KindOp, Op: "and", Args: args}
			if len(args) == 1 {
				when = args[0]
			} else if len(args) == 0 {
				when = &Expr{Kind: KindLiteral, Lit: &Literal{Kind: "boolean", Value: true}}
			}

			rules = append(rules, contracts.Rule{
				RuleID:   ruleID,
				Priority: int64(total-seq+1) * 10,
				When:     mustJSON(when),
				Then: contracts.Then{
					Decision:      t.Then.Decision,
					ReasonCode:    t.Then.ReasonCode,
					ReasonMessage: t.Then.ReasonMessage,
					Assign: []contracts.Assign{{
						Var:  t.Target.Var,
						Expr: mustJSON(&Expr{Kind: KindLiteral, Lit: lit}),
					}},
				},
			})
		}
	}
	return rules
}

// expandCrossAxis turns one axis' cells into per-line condition expressions.
//
// Every line must give a concrete condition for every header fact: a wildcard here would mean "this axis does not
// constrain", which turns the matrix back into a one-dimensional table and makes the visual promise a lie.
func expandCrossAxis(
	cells []map[string]json.RawMessage,
	headers []TableColumn,
	axisPath, axisLabel string,
	d *diag,
) [][]*Expr {
	out := make([][]*Expr, len(cells))
	seenLine := map[string]int{} // canonical line → first line index
	for i, line := range cells {
		args := make([]*Expr, 0, len(headers))
		for _, h := range headers {
			if h.Fact == "" {
				continue // already reported
			}
			path := fmt.Sprintf("crossTable.%s[%d][%s]", axisPath, i, h.Fact)
			raw, ok := line[h.Fact]
			if !ok {
				d.errorf(CodeTableInvalid, path, "", h.Fact,
					fmt.Sprintf("第 %d %s缺少条件列 %s 的取值", i+1, axisLabel, h.Fact),
					"交叉表的每个维度格子都必须给出具体条件")
				continue
			}
			if isWildcard(raw) {
				d.errorf(CodeTableInvalid, path, "", h.Fact,
					"交叉表不支持通配 *", "需要通配说明这张表该用普通决策表")
				continue
			}
			if e := cellExpr(h, raw, path, "", d); e != nil {
				args = append(args, e)
			}
		}
		out[i] = args

		// Duplicates are checked per *line*, not per fact: with several header facts two lines may repeat
		// one fact and still differ in another, which is a legitimate matrix. Only a line whose whole
		// condition set repeats another line is guaranteed to be unreachable.
		key := lineKey(line, headers)
		if key == "" {
			continue
		}
		if prev, dup := seenLine[key]; dup {
			d.errorf(CodeTableInvalid, fmt.Sprintf("crossTable.%s[%d]", axisPath, i), "", "",
				fmt.Sprintf("第 %d %s的条件与第 %d %s完全相同", i+1, axisLabel, prev+1, axisLabel),
				"两行条件一样，其中一格的规则永远命中不到")
			continue
		}
		seenLine[key] = i
	}
	return out
}

// lineKey canonicalises one axis line so two lines with the same conditions compare equal
// regardless of key order or spacing.
func lineKey(line map[string]json.RawMessage, headers []TableColumn) string {
	parts := make([]string, 0, len(headers))
	for _, h := range headers {
		if h.Fact == "" {
			continue
		}
		raw, ok := line[h.Fact]
		if !ok {
			continue
		}
		parts = append(parts, h.Fact+"="+canonicalJSON(raw))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// crossCellLiteral reads one matrix cell. Both `{"type":"integer","value":5}` (the TypedLiteral the UI writes) and a
// bare `5` are accepted; the target type is what actually decides the literal kind, so a disagreeing `type` is an error
// rather than a silent reinterpretation.
func crossCellLiteral(raw json.RawMessage, targetType string) (*Literal, error) {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("格子值无法解析: %v", err)
	}
	val := parsed
	// Only a typed wrapper is unwrapped. A `{op,value}` object is *not* a literal - a cell is a value, not a
	// comparison - so it falls through to coerceLiteral and fails with the object's type named in the message.
	if obj, ok := parsed.(map[string]any); ok {
		if declared, hasType := obj["type"].(string); hasType {
			if _, hasValue := obj["value"]; hasValue && len(obj) <= 2 {
				if declared != "" && declared != targetType {
					return nil, fmt.Errorf("格子声明为 %s，但赋值目标是 %s", declared, targetType)
				}
				val = obj["value"]
			}
		}
	}
	lit, err := coerceLiteral(literalKindFor(targetType), val)
	if err != nil {
		return nil, fmt.Errorf("格子值不能按 %s 解释: %v", targetType, err)
	}
	return lit, nil
}

// isEmptyCell marks a cell with no rule. `null` is what the UI writes for a cleared cell; an absent array element
// decodes to an empty RawMessage and is treated the same way.
func isEmptyCell(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// canonicalJSON normalises a cell so two cells that mean the same thing compare equal regardless of key order or spacing.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
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
