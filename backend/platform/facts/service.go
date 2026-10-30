package facts

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/persist"
)

// RefChecker answers "which rule sets reference a given fact". It is wired by httpapi to the rule set repository,
// so that the facts package does not depend back on the dsl package (dependencies flow one way, A-1).
type RefChecker func(fact string) []string

// Service is the fact catalog service.
type Service struct {
	st   *persist.Store[Catalog]
	refs RefChecker
	now  func() time.Time
}

// New builds the service.
func New(st *persist.Store[Catalog]) *Service {
	return &Service{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// WithRefChecker attaches the reference check (used before deletion).
func (s *Service) WithRefChecker(fn RefChecker) *Service {
	s.refs = fn
	return s
}

// WithClock injects the clock (for deterministic tests).
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// Catalog returns a shallow copy of the catalog document (the slice is copied; the element value type holds no pointers, so it is safe to hand out).
func (s *Service) Catalog() Catalog {
	var out Catalog
	s.st.View(func(c *Catalog) {
		out = *c
		out.Facts = append([]contracts.FactSchema(nil), c.Facts...)
	})
	return out
}

// Snapshot returns a `FactCatalog` usable for compilation (`contracts/02` §1).
func (s *Service) Snapshot() contracts.FactCatalog {
	c := s.Catalog()
	return contracts.FactCatalog{CatalogVersion: c.CatalogVersion, Facts: c.Facts}
}

// List filters by namespace/keyword (empty = no filtering) and excludes deprecated fields by default.
func (s *Service) List(namespace, q string, includeDeprecated bool) []contracts.FactSchema {
	c := s.Catalog()
	out := make([]contracts.FactSchema, 0, len(c.Facts))
	q = strings.ToLower(strings.TrimSpace(q))
	for _, f := range c.Facts {
		if !includeDeprecated && f.Deprecated {
			continue
		}
		if namespace != "" && !strings.HasPrefix(f.Fact, namespace+".") {
			continue
		}
		if q != "" {
			hay := strings.ToLower(f.Fact + " " + f.DisplayName + " " + f.Semantics)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// Get fetches a single fact.
func (s *Service) Get(name string) (contracts.FactSchema, bool) {
	c := s.Catalog()
	for _, f := range c.Facts {
		if f.Fact == name {
			return f, true
		}
	}
	return contracts.FactSchema{}, false
}

// Create adds a fact. It returns the stored fact + diagnostics (including warnings).
func (s *Service) Create(actor string, in contracts.FactSchema) (contracts.FactSchema, []Issue, error) {
	in = Normalize(in)
	var issues []Issue
	err := s.st.Update(func(c *Catalog) error {
		issues = Validate(in, Index(c.Facts))
		if HasError(issues) {
			return &ValidationError{Issues: issues}
		}
		c.Facts = append(c.Facts, in)
		SortFacts(c.Facts)
		s.touch(c, actor)
		return nil
	})
	if err != nil {
		return contracts.FactSchema{}, issues, err
	}
	return in, issues, nil
}

// Update modifies a fact. The fact name cannot be changed (a rename = delete one and add one, which must explicitly go through both paths,
// otherwise consumers would silently lose the field).
func (s *Service) Update(actor, name string, in contracts.FactSchema) (contracts.FactSchema, []Issue, error) {
	in = Normalize(in)
	var issues []Issue
	err := s.st.Update(func(c *Catalog) error {
		pos := -1
		for i := range c.Facts {
			if c.Facts[i].Fact == name {
				pos = i
				break
			}
		}
		if pos < 0 {
			return &NotFoundError{Fact: name}
		}
		old := c.Facts[pos]
		if in.Fact == "" {
			in.Fact = old.Fact
		}
		if in.Fact != name {
			return &ValidationError{Issues: []Issue{{
				Code: CodeNameInvalid, Level: "error", Path: "fact", Fact: name,
				Message: "事实名不允许修改（改名等价于删旧建新）",
				Suggest: "保持 fact 不变，另建一个新事实并废弃旧的",
			}}}
		}

		// index excluding the fact itself, so it is not flagged as its own duplicate.
		others := make([]contracts.FactSchema, 0, len(c.Facts)-1)
		others = append(others, c.Facts[:pos]...)
		others = append(others, c.Facts[pos+1:]...)
		issues = Validate(in, Index(others))

		if breakingChange(old, in) && in.Version == old.Version {
			issues = append(issues, Issue{
				Code: CodeVersionInvalid, Level: "error", Path: "version", Fact: name,
				Message: "改类型 / 改枚举 / 取消必填属破坏性变更，必须升 version（contracts/02 §7）",
				Suggest: fmt.Sprintf("把 version 从 %s 提到下一个值", old.Version),
			})
		}
		if HasError(issues) {
			return &ValidationError{Issues: issues}
		}

		c.Facts[pos] = in
		SortFacts(c.Facts)
		s.touch(c, actor)
		return nil
	})
	if err != nil {
		return contracts.FactSchema{}, issues, err
	}
	return in, issues, nil
}

// Deprecate marks a fact deprecated and names its replacement (`contracts/02` §7: warn during the deprecation window, refuse breaking removals).
func (s *Service) Deprecate(actor, name, replacedBy string) (contracts.FactSchema, []Issue, error) {
	var issues []Issue
	var out contracts.FactSchema
	err := s.st.Update(func(c *Catalog) error {
		pos := -1
		for i := range c.Facts {
			if c.Facts[i].Fact == name {
				pos = i
				break
			}
		}
		if pos < 0 {
			return &NotFoundError{Fact: name}
		}
		f := c.Facts[pos]
		f.Deprecated = true
		f.ReplacedBy = replacedBy
		issues = Validate(f, Index(removeAt(c.Facts, pos)))
		if HasError(issues) {
			return &ValidationError{Issues: issues}
		}
		c.Facts[pos] = f
		s.touch(c, actor)
		out = f
		return nil
	})
	return out, issues, err
}

// Delete removes a fact. It is refused while rule sets reference it (otherwise rule packages would silently lose the field).
//
// Note: the contract's "deprecate" takes precedence over "delete" — production should go through Deprecate; Delete targets
// cleaning up fields created by mistake, hence the no-reference requirement.
func (s *Service) Delete(actor, name string) error {
	return s.st.Update(func(c *Catalog) error {
		pos := -1
		for i := range c.Facts {
			if c.Facts[i].Fact == name {
				pos = i
				break
			}
		}
		if pos < 0 {
			return &NotFoundError{Fact: name}
		}
		if s.refs != nil {
			if referers := s.refs(name); len(referers) > 0 {
				return &InUseError{Fact: name, Referers: referers}
			}
		}
		c.Facts = removeAt(c.Facts, pos)
		s.touch(c, actor)
		return nil
	})
}

// touch advances the revision number and the catalog version. Every change must record both, otherwise artifacts cannot tell whether they are stale.
func (s *Service) touch(c *Catalog, actor string) {
	now := s.now()
	c.Revision++
	c.CatalogVersion = CatalogVersion(now, c.Revision)
	c.UpdatedAt = now.Format(time.RFC3339)
	c.UpdatedBy = actor
}

func removeAt(list []contracts.FactSchema, pos int) []contracts.FactSchema {
	out := make([]contracts.FactSchema, 0, len(list)-1)
	out = append(out, list[:pos]...)
	out = append(out, list[pos+1:]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Fact < out[j].Fact })
	return out
}

// breakingChange determines whether a breaking change happened between two versions (`contracts/02` §7).
func breakingChange(old, updated contracts.FactSchema) bool {
	if old.Type != updated.Type {
		return true
	}
	if old.Required && !updated.Required {
		return true
	}
	// removing an enum value is breaking (adding values is compatible).
	for _, v := range old.Enum {
		if !contains(updated.Enum, v) {
			return true
		}
	}
	return false
}
