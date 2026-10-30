package facts

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/persist"
)

// RefChecker 查询「某事实被哪些规则集引用」。由 httpapi 接线到规则集仓库，
// 这样 facts 包不反向依赖 dsl 包（依赖方向单向，A-1）。
type RefChecker func(fact string) []string

// Service 事实目录服务。
type Service struct {
	st   *persist.Store[Catalog]
	refs RefChecker
	now  func() time.Time
}

// New 构造服务。
func New(st *persist.Store[Catalog]) *Service {
	return &Service{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// WithRefChecker 挂上引用检查（删除前用）。
func (s *Service) WithRefChecker(fn RefChecker) *Service {
	s.refs = fn
	return s
}

// WithClock 注入时钟（测试确定性）。
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// Catalog 返回目录文档的浅拷贝（切片复制，元素值类型不含指针，可安全外传）。
func (s *Service) Catalog() Catalog {
	var out Catalog
	s.st.View(func(c *Catalog) {
		out = *c
		out.Facts = append([]contracts.FactSchema(nil), c.Facts...)
	})
	return out
}

// Snapshot 返回可用于编译的 `FactCatalog`（`contracts/02` §1）。
func (s *Service) Snapshot() contracts.FactCatalog {
	c := s.Catalog()
	return contracts.FactCatalog{CatalogVersion: c.CatalogVersion, Facts: c.Facts}
}

// List 按命名空间/关键字过滤（空 = 不过滤），默认不含已废弃字段。
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

// Get 取单个事实。
func (s *Service) Get(name string) (contracts.FactSchema, bool) {
	c := s.Catalog()
	for _, f := range c.Facts {
		if f.Fact == name {
			return f, true
		}
	}
	return contracts.FactSchema{}, false
}

// Create 新增事实。返回落库事实 + 诊断（含 warn）。
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

// Update 修改事实。事实名不可改（改名 = 删一个加一个，必须显式走两条路径，
// 否则消费方会在毫不知情的情况下丢掉字段）。
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

		// 除自身之外的索引，避免把自己判成重复。
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

// Deprecate 标记废弃并给出替换字段（`contracts/02` §7：废弃期告警、破坏性移除拒绝）。
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

// Delete 删除事实。被规则集引用时拒绝（否则规则包会静默失去字段）。
//
// 注意：契约的「弃用」优先于「删除」——生产环境应走 Deprecate；Delete 面向
// 建错字段的清理，所以要求无引用。
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

// touch 推进修订号与目录版本。所有变更都必须留下这两笔，否则生成物无法判断是否过期。
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

// breakingChange 判定两次版本之间是否发生了破坏性变更（`contracts/02` §7）。
func breakingChange(old, updated contracts.FactSchema) bool {
	if old.Type != updated.Type {
		return true
	}
	if old.Required && !updated.Required {
		return true
	}
	// 枚举删值 = 破坏性（新增值是兼容的）。
	for _, v := range old.Enum {
		if !contains(updated.Enum, v) {
			return true
		}
	}
	return false
}
