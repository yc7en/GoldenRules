package dsl

import (
	"sort"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/persist"
)

// Repo 规则集仓库文档。
type Repo struct {
	Items []Definition `json:"items"`
}

// Store 规则集仓库。P0 不做「草稿/已发布」双态：规则集的编辑是就地编辑，
// 版本与状态由 B8 的规则包承担（规则包才是原子发布单元）。
type Store struct {
	st  *persist.Store[Repo]
	now func() time.Time
}

// NewStore 打开仓库。
func NewStore(st *persist.Store[Repo]) *Store {
	return &Store{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入时钟。
func (s *Store) WithClock(fn func() time.Time) *Store {
	s.now = fn
	return s
}

// Get 取单个规则集。
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

// List 按场景过滤（空 = 全部），按 rulesetId 升序。
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

// Put 新增或整体替换一个规则集。调用方必须**先**用 Analyze 校验过；
// 仓库不做业务校验（它不知道事实目录）。
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

// Delete 删除规则集。
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

// ReferencingFact 返回引用了某事实的规则集 ID（给事实目录的删除保护用）。
//
// 只做**语法层**扫描：把 when/assign 与决策表列解析成 AST 再取 Facts，
// 因此拼错的字段名也会被算作引用（宁可多拦，不可漏放）。
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

// NotExistError 资源不存在。
type NotExistError struct {
	What string
	ID   string
}

func (e *NotExistError) Error() string { return e.What + "不存在: " + e.ID }

// OutputSchemas 汇总各规则集携带的输出 schema（B8 组装规则包时用）。
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
