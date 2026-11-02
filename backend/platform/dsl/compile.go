package dsl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"goldenrules/platform/contracts"
)

// compile builds the IR: fact references → slot indices, and also produces the slot table and two digests.
//
// Determinism sources (`contracts/01` §5.1 "same DSL + same Fact Catalog version → byte-identical IR"):
//  1. rules are first sorted by `(priority desc, ruleId asc)` (matching engine load order), independent of authoring order;
//  2. slot indices are assigned in the **sorted** rule order, when before assign, on first appearance in depth-first order;
//  3. every JSON payload is encoded from a map/struct, so key order is fixed.
//
// Hence "merely reordering the rules" does not change the IR bytes, but "changing a priority" does — this is intentional:
// priority affects semantics, so the artifact should change.
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

	strategy := effectiveStrategy(def.HitStrategy)
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

// dslDoc is the canonical form of "what the author wrote" — the digest is based on it, not on the IR.
// That way "DSL unchanged but artifact changed" is detected immediately (sourceHash is compared at publish time).
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
