package dsl

import (
	"sort"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/persist"
)

// Repo is the rule set repository document.
type Repo struct {
	Items []Definition `json:"items"`
}

// Store is the rule set repository. P0 does not do a "draft/published" two-state model: rule sets are edited in place,
// and version and status are owned by the B8 rule package (the rule package is the atomic publish unit).
type Store struct {
	st  *persist.Store[Repo]
	now func() time.Time
}

// NewStore opens the repository.
func NewStore(st *persist.Store[Repo]) *Store {
	return &Store{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock injects the clock.
func (s *Store) WithClock(fn func() time.Time) *Store {
	s.now = fn
	return s
}

// Get fetches a single rule set.
func (s *Store) Get(id string) (Definition, bool) {
	var out Definition
	found := false
	s.st.View(func(r *Repo) {
		for _, d := range r.Items {
			if d.RulesetID == id {
				out, found = d, true
				return
			}
		}
	})
	return out, found
}

// List filters by scene (empty = all) and sorts by rulesetId ascending.
func (s *Store) List(scene string) []Definition {
	var out []Definition
	s.st.View(func(r *Repo) {
		for _, d := range r.Items {
			if scene != "" && d.Scene != scene {
				continue
			}
			out = append(out, d)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].RulesetID < out[j].RulesetID })
	return out
}

// Put inserts or fully replaces a rule set. Callers must have validated with Analyze **first**;
// the repository performs no business validation (it does not know the fact catalog).
func (s *Store) Put(actor string, def Definition) (Definition, error) {
	var saved Definition
	err := s.st.Update(func(r *Repo) error {
		pos := -1
		for i := range r.Items {
			if r.Items[i].RulesetID == def.RulesetID {
				pos = i
				break
			}
		}
		rev := int64(1)
		if pos >= 0 {
			rev = r.Items[pos].Revision + 1
		}
		def.Revision = rev
		def.UpdatedAt = s.now().Format(time.RFC3339)
		def.UpdatedBy = actor
		if pos >= 0 {
			r.Items[pos] = def
		} else {
			r.Items = append(r.Items, def)
		}
		saved = def
		return nil
	})
	return saved, err
}

// Delete removes a rule set.
func (s *Store) Delete(id string) error {
	return s.st.Update(func(r *Repo) error {
		for i := range r.Items {
			if r.Items[i].RulesetID == id {
				r.Items = append(r.Items[:i], r.Items[i+1:]...)
				return nil
			}
		}
		return &NotExistError{What: "规则集", ID: id}
	})
}

// ReferencingFact returns the IDs of rule sets that reference a given fact (used by the fact catalog's delete guard).
//
// It only scans at the **syntactic level**: when/assign and decision-table columns are parsed into an AST and then Facts is taken,
// so even misspelled field names count as references (better to over-block than to let something slip).
func (s *Store) ReferencingFact(fact string) []string {
	var out []string
	for _, def := range s.List("") {
		if definitionUsesFact(def, fact) {
			out = append(out, def.RulesetID)
		}
	}
	sort.Strings(out)
	return out
}

func definitionUsesFact(def Definition, fact string) bool {
	if def.Table != nil {
		for _, c := range def.Table.Columns {
			if c.Fact == fact {
				return true
			}
		}
	}
	silent := &diag{}
	for _, r := range def.Rules {
		if e := ParseWhen(r.When, "", silent); e != nil {
			for _, f := range e.Facts() {
				if f == fact {
					return true
				}
			}
		}
		for _, a := range r.Then.Assign {
			if e := ParseValue(a.Expr, "", silent); e != nil {
				for _, f := range e.Facts() {
					if f == fact {
						return true
					}
				}
			}
		}
	}
	return false
}

// NotExistError means the resource does not exist.
type NotExistError struct {
	What string
	ID   string
}

func (e *NotExistError) Error() string { return e.What + "不存在: " + e.ID }

// OutputSchemas collects the output schemas carried by each rule set (used by B8 when assembling a rule package).
func (s *Store) OutputSchemas() []contracts.OutputSchema {
	seen := map[string]bool{}
	var out []contracts.OutputSchema
	for _, d := range s.List("") {
		if d.OutputSchema == nil {
			continue
		}
		key := d.OutputSchema.OutputSchemaID + "@" + d.OutputSchema.OutputSchemaVersion
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, *d.OutputSchema)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OutputSchemaID != out[j].OutputSchemaID {
			return out[i].OutputSchemaID < out[j].OutputSchemaID
		}
		return out[i].OutputSchemaVersion < out[j].OutputSchemaVersion
	})
	return out
}
