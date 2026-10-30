package dsl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"goldenrules/platform/contracts"
)

// compile 生成 IR：字段引用 → slot 索引，同时产出 slot 表与两个摘要。
//
// 确定性来源（`contracts/01` §5.1「相同 DSL + 相同 Fact Catalog 版本 → 字节一致的 IR」）：
//  1. 规则先按 `(priority desc, ruleId asc)` 排序（与引擎加载顺序一致），排序结果与作者书写顺序无关；
//  2. slot 编号按**排序后**的规则顺序、先 when 后 assign、深度优先首次出现分配；
//  3. 所有 JSON 由 map/结构体编码，键序固定。
//
// 所以「只调换了规则的书写顺序」不会改变 IR 字节，但「改了优先级」会——这是有意的：
// 优先级影响语义，产物就该变。
func compile(def Definition, asts []ruleAST, cat map[string]contracts.FactSchema) (
	*contracts.RuleSet, []contracts.SlotDef, string, string,
) {
	sort.SliceStable(asts, func(i, j int) bool {
		if asts[i].Rule.Priority != asts[j].Rule.Priority {
			return asts[i].Rule.Priority > asts[j].Rule.Priority
		}
		return asts[i].Rule.RuleID < asts[j].Rule.RuleID
	})

	slotIndex := map[string]int{}
	var slots []contracts.SlotDef

	var resolve func(*Expr) *Expr
	resolve = func(e *Expr) *Expr {
		if e == nil {
			return nil
		}
		if e.Kind == KindFact {
			idx, ok := slotIndex[e.Fact]
			if !ok {
				idx = len(slots)
				slotIndex[e.Fact] = idx
				fs := cat[e.Fact]
				v := fs.Version
				if v == "" {
					v = "v1"
				}
				slots = append(slots, contracts.SlotDef{Name: e.Fact, Type: fs.Type, FactVersion: v})
			}
			return &Expr{Kind: KindSlot, Slot: idx}
		}
		if e.Kind == KindOp {
			args := make([]*Expr, 0, len(e.Args))
			for _, a := range e.Args {
				args = append(args, resolve(a))
			}
			return &Expr{Kind: KindOp, Op: e.Op, Args: args, Negated: e.Negated, Func: e.Func}
		}
		return e
	}

	irRules := make([]contracts.Rule, 0, len(asts))
	for _, a := range asts {
		then := a.Rule.Then
		then.Assign = make([]contracts.Assign, 0, len(a.Assigns))
		for _, as := range a.Assigns {
			then.Assign = append(then.Assign, contracts.Assign{Var: as.Var, Expr: mustJSON(resolve(as.Expr))})
		}
		scene := a.Rule.Scene
		if scene == "" {
			scene = def.Scene
		}
		irRules = append(irRules, contracts.Rule{
			RuleID:    a.Rule.RuleID,
			Version:   a.Rule.Version,
			Scene:     scene,
			Priority:  a.Rule.Priority,
			RiskLevel: a.Rule.RiskLevel,
			When:      mustJSON(resolve(a.When)),
			Then:      then,
		})
	}

	strategy := def.HitStrategy
	if strategy == "" {
		strategy = "ALL"
	}
	ir := &contracts.RuleSet{
		RulesetID:           def.RulesetID,
		Version:             def.Version,
		Scene:               def.Scene,
		HitStrategy:         strategy,
		DefaultDecision:     def.DefaultDecision,
		OutputSchemaID:      def.OutputSchemaID,
		OutputSchemaVersion: def.OutputSchemaVersion,
		Rules:               irRules,
		SlotTable:           slots,
	}

	return ir, slots, sourceHash(def, asts), hashOf(slots)
}

// dslDoc 是「作者写了什么」的规范形态——摘要基于它，而不是基于 IR。
// 这样「DSL 没改但产物变了」能立刻被发现（发布期比对 sourceHash）。
type dslDoc struct {
	RulesetID       string          `json:"rulesetId"`
	Scene           string          `json:"scene"`
	HitStrategy     string          `json:"hitStrategy"`
	DefaultDecision string          `json:"defaultDecision"`
	OutputSchemaID  string          `json:"outputSchemaId,omitempty"`
	OutputSchemaVer string          `json:"outputSchemaVersion,omitempty"`
	Rules           []dslRuleInHash `json:"rules"`
}

type dslRuleInHash struct {
	RuleID   string          `json:"ruleId"`
	Priority int64           `json:"priority"`
	When     json.RawMessage `json:"when"`
	Then     contracts.Then  `json:"then"`
}

func sourceHash(def Definition, asts []ruleAST) string {
	doc := dslDoc{
		RulesetID:       def.RulesetID,
		Scene:           def.Scene,
		HitStrategy:     def.HitStrategy,
		DefaultDecision: def.DefaultDecision,
		OutputSchemaID:  def.OutputSchemaID,
		OutputSchemaVer: def.OutputSchemaVersion,
	}
	for _, a := range asts {
		then := a.Rule.Then
		then.Assign = make([]contracts.Assign, 0, len(a.Assigns))
		for _, as := range a.Assigns {
			then.Assign = append(then.Assign, contracts.Assign{Var: as.Var, Expr: mustJSON(as.Expr)})
		}
		doc.Rules = append(doc.Rules, dslRuleInHash{
			RuleID:   a.Rule.RuleID,
			Priority: a.Rule.Priority,
			When:     mustJSON(a.When),
			Then:     then,
		})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return hashBytes(body)
}

func hashOf(slots []contracts.SlotDef) string {
	body, err := json.Marshal(slots)
	if err != nil {
		return ""
	}
	return hashBytes(body)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
