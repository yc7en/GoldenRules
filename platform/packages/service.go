package packages

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"goldenrules/platform/dsl"
	"goldenrules/platform/persist"
)

// Service 规则包仓库服务。
type Service struct {
	st  *persist.Store[Repo]
	now func() time.Time
}

// NewService 构造服务。
func NewService(st *persist.Store[Repo]) *Service {
	return &Service{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入时钟（测试确定性；也用于验证「定时发布」）。
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// List 列出某 packageId 的所有版本（按版本号升序）；packageID 为空时列全部。
func (s *Service) List(packageID string) []Version {
	var out []Version
	s.st.View(func(r *Repo) {
		for _, v := range r.Versions {
			if packageID != "" && v.PackageID != packageID {
				continue
			}
			out = append(out, v.Clone())
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].PackageID != out[j].PackageID {
			return out[i].PackageID < out[j].PackageID
		}
		return versionLess(out[i].PackageVersion, out[j].PackageVersion)
	})
	return out
}

// Get 取指定版本。
func (s *Service) Get(packageID, version string) (Version, bool) {
	var out Version
	found := false
	s.st.View(func(r *Repo) {
		for _, v := range r.Versions {
			if v.PackageID == packageID && v.PackageVersion == version {
				out, found = v.Clone(), true
				return
			}
		}
	})
	return out, found
}

// Active 取当前生效版本。
//
// 注意「生效」≠「published」：可以先发布但 `validFrom` 在未来（`contracts/06` §3）。
// 指针指向的版本若还没到生效时间，这里依然返回它，并带 `IsEffective=false` 供调用方判断。
func (s *Service) Active(packageID string) (Version, bool) {
	var (
		out    Version
		found  bool
		active string
	)
	s.st.View(func(r *Repo) {
		active = r.Active[packageID]
		for _, v := range r.Versions {
			if v.PackageID == packageID && v.PackageVersion == active {
				out, found = v.Clone(), true
				return
			}
		}
	})
	return out, found
}

// CreateDraft 编译并写入一个草稿版本。
//
// 版本号：调用方没给就自动取下一个；同名版本已存在时——草稿可覆盖，已批准/已发布一律拒绝
// （`contracts/06` §2.2 不可变）。
func (s *Service) CreateDraft(in BuildInput) (Version, []dsl.Report, error) {
	if in.Now.IsZero() {
		in.Now = s.now()
	}

	var (
		built    Version
		reports  []dsl.Report
		buildErr error
	)
	err := s.st.Update(func(r *Repo) error {
		version := in.Version
		if version == "" {
			version = NextVersion(versionsOf(r, in.PackageID))
			in.Version = version
		}
		for _, v := range r.Versions {
			if v.PackageID == in.PackageID && v.PackageVersion == version && v.Status != StatusDraft {
				return &StateError{
					Code: CodeVersionExists,
					Message: fmt.Sprintf("%s@%s 已处于 %s 状态，版本不可变；请换一个版本号",
						in.PackageID, version, v.Status),
				}
			}
		}

		built, reports, buildErr = Build(in)
		if buildErr != nil {
			return buildErr
		}

		replaced := false
		for i := range r.Versions {
			if r.Versions[i].PackageID == in.PackageID && r.Versions[i].PackageVersion == version {
				r.Versions[i] = built
				replaced = true
				break
			}
		}
		if !replaced {
			r.Versions = append(r.Versions, built)
		}
		return nil
	})
	return built, reports, err
}

// Approve 草稿 → 已批准（`contracts/06` §4.1）。
//
// 审批矩阵与职责分离在 B9；这一步只负责状态推进，B9 会在调用它之前先走矩阵。
func (s *Service) Approve(packageID, version, actor string) (Version, error) {
	var out Version
	err := s.st.Update(func(r *Repo) error {
		v, err := find(r, packageID, version)
		if err != nil {
			return err
		}
		if v.Status != StatusDraft {
			return &StateError{Code: CodeBadTransition,
				Message: fmt.Sprintf("只有 draft 可以批准，当前是 %s", v.Status)}
		}
		v.Status = StatusApproved
		out = v.Clone()
		return nil
	})
	return out, err
}

// Publish 已批准 → 已发布；并把上一个生效版本退役（`contracts/06` §4.2 取代）。
//
// 同一版本重复发布是幂等操作（§4.2），不产生新版本号、不重复改状态。
// 支持「定时发布」：`validFrom` 在未来时，包处于 published 但尚未生效。
func (s *Service) Publish(packageID, version, actor, validFrom, validUntil string) (Version, error) {
	var out Version
	err := s.st.Update(func(r *Repo) error {
		v, err := find(r, packageID, version)
		if err != nil {
			return err
		}

		// 幂等：已经是当前生效版本，再发一次什么都不做
		if v.Status == StatusPublished && r.Active[packageID] == version {
			out = v.Clone()
			return nil
		}
		if v.Status != StatusApproved {
			return &StateError{Code: CodeNotApproved,
				Message: fmt.Sprintf("只有 approved 可以发布，%s 当前是 %s", version, v.Status)}
		}

		now := s.now().UTC().Format(time.RFC3339)
		prev := r.Active[packageID]
		if prev != "" && prev != version {
			if old, err := find(r, packageID, prev); err == nil && old.Status == StatusPublished {
				old.Status = StatusRetired
				old.RetiredAt = now
				old.RetiredCause = "SUPERSEDED"
				old.SupersededBy = version
			}
		}

		v.Status = StatusPublished
		v.PublishedAt = now
		v.PublishedBy = actor
		v.ValidFrom = validFrom
		v.ValidUntil = validUntil
		r.ensureActive()
		r.Active[packageID] = version
		out = v.Clone()
		return nil
	})
	return out, err
}

// Rollback 把生效版本切回某个旧版本（`contracts/06` §6）。
//
// 语义要点：
//   - **不产生新版本号**，只是一次版本选择变更；
//   - 唯一的「逆向流转」：被切回的旧版本从 retired 恢复为 published。
//     契约 §4.2 说状态机单向，§6 又要求回滚可用——这里按 §6 实现，并在审计里记明原因，
//     除此之外任何 published → draft 之类的回退一律拒绝。
//   - 当前生效版本标记 `retired(ROLLED_BACK)` 并记 `supersededBy`。
func (s *Service) Rollback(packageID, toVersion, actor, reason string) (Version, error) {
	var out Version
	err := s.st.Update(func(r *Repo) error {
		current := r.Active[packageID]
		if current == "" {
			return &StateError{Code: CodeNoActive, Message: packageID + " 还没有生效版本，无从回滚"}
		}
		if current == toVersion {
			return &StateError{Code: CodeRollbackTarget,
				Message: toVersion + " 已经是当前生效版本"}
		}
		target, err := find(r, packageID, toVersion)
		if err != nil {
			return err
		}
		if target.PublishedAt == "" {
			return &StateError{Code: CodeRollbackTarget,
				Message: fmt.Sprintf("%s 从未发布过，不能作为回滚目标", toVersion)}
		}

		now := s.now().UTC().Format(time.RFC3339)
		if cur, err := find(r, packageID, current); err == nil {
			cur.Status = StatusRetired
			cur.RetiredAt = now
			cur.RetiredCause = "ROLLED_BACK"
			cur.SupersededBy = toVersion
		}
		target.Status = StatusPublished
		target.RetiredAt = ""
		target.RetiredCause = ""
		target.SupersededBy = ""
		r.ensureActive()
		r.Active[packageID] = toVersion
		out = target.Clone()
		return nil
	})
	return out, err
}

// Retire 手动退役（`validUntil` 到期之外的显式下线）。
func (s *Service) Retire(packageID, version, actor, reason string) (Version, error) {
	var out Version
	err := s.st.Update(func(r *Repo) error {
		v, err := find(r, packageID, version)
		if err != nil {
			return err
		}
		if v.Status != StatusPublished {
			return &StateError{Code: CodeBadTransition,
				Message: fmt.Sprintf("只有 published 可以退役，当前是 %s", v.Status)}
		}
		if r.Active[packageID] == version {
			return &StateError{Code: CodeStillServing,
				Message: version + " 仍在生效，请先发布新版本或回滚"}
		}
		v.Status = StatusRetired
		v.RetiredAt = s.now().UTC().Format(time.RFC3339)
		v.RetiredCause = reason
		out = v.Clone()
		return nil
	})
	return out, err
}

// Artifact 返回交付给决策服务的规则包 JSON（IR 形态）。
//
// 这是平台与引擎之间**唯一**的产物交接面（`ARCHITECTURE.md` §2.1「只通过规则包产物 + 契约对接」）。
func (v Version) Artifact() ([]byte, error) {
	return marshalCanonical(v.Package)
}

// ---------------------------------------------------------------------------

func find(r *Repo, packageID, version string) (*Version, error) {
	for i := range r.Versions {
		if r.Versions[i].PackageID == packageID && r.Versions[i].PackageVersion == version {
			return &r.Versions[i], nil
		}
	}
	return nil, &NotFoundError{PackageID: packageID, Version: version}
}

func versionsOf(r *Repo, packageID string) []string {
	var out []string
	for _, v := range r.Versions {
		if v.PackageID == packageID {
			out = append(out, v.PackageVersion)
		}
	}
	return out
}

// versionLess 版本号比较（整数式 vN 与语义化都支持）。
func versionLess(a, b string) bool {
	if na, ok := parseIntStyle(a); ok {
		if nb, ok2 := parseIntStyle(b); ok2 {
			return na < nb
		}
	}
	if sa, ok := parseSemver(a); ok {
		if sb, ok2 := parseSemver(b); ok2 {
			return lessSemver(sa, sb)
		}
	}
	return a < b
}

func marshalCanonical(v any) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
