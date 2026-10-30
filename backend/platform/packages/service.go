package packages

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"goldenrules/platform/dsl"
	"goldenrules/platform/persist"
)

// Service is the rule package repository service.
type Service struct {
	st  *persist.Store[Repo]
	now func() time.Time
	// verifier runs the publish gate over approved test cases; nil means "this process has no executor".
	verifier TestCaseVerifier
}

// NewService builds the service.
func NewService(st *persist.Store[Repo]) *Service {
	return &Service{st: st, now: func() time.Time { return time.Now().UTC() }}
}

// WithVerifier attaches the test case gate executor (F-PKG06).
func (s *Service) WithVerifier(v TestCaseVerifier) *Service {
	s.verifier = v
	return s
}

// WithClock injects a clock (test determinism; also used to verify "scheduled publish").
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// List lists all versions of one packageId (ascending by version); an empty packageID lists everything.
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

// Get returns the given version.
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

// Active returns the currently effective version.
//
// Note that "effective" ≠ "published": a package may be published with `validFrom` in the future (`contracts/06` §3).
// If the pointed-to version has not reached its effective time yet, it is still returned, carrying `IsEffective=false` for the caller to judge.
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

// CreateDraft compiles and writes one draft version.
//
// Version number: auto-incremented when the caller does not supply one; when a version with the same name exists — a draft may be overwritten,
// while approved/published ones are always refused (`contracts/06` §2.2 immutability).
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

// Approve moves draft → approved (`contracts/06` §4.1).
//
// The approval matrix and segregation of duties live in B9; this step only advances the state, and B9 walks the matrix before calling it.
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

// Publish moves approved → published, and retires the previously effective version (`contracts/06` §4.2 supersession).
//
// Re-publishing the same version is idempotent (§4.2): no new version number, no repeated state change.
// Scheduled publish is supported: with `validFrom` in the future the package is published but not yet effective.
func (s *Service) Publish(packageID, version, actor, validFrom, validUntil string) (Version, error) {
	// The test case gate runs outside the store lock: it is an external call (engine execution) and must not be squeezed into the store update's critical section,
	// otherwise a slow gate would block every read and write of the whole package repository.
	if err := s.runPublishGate(packageID, version); err != nil {
		return Version{}, err
	}
	var out Version
	err := s.st.Update(func(r *Repo) error {
		v, err := find(r, packageID, version)
		if err != nil {
			return err
		}

		// Idempotent: already the current effective version, publishing again does nothing
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

// Rollback switches the effective version back to an older one (`contracts/06` §6).
//
// Semantic points:
//   - **no new version number is created**; this is only a change of version selection;
//   - the one and only "reverse transition": the version switched back to recovers from retired to published.
//     Contract §4.2 says the state machine is one-way, while §6 requires rollback to work — §6 is implemented here, with the reason recorded in the audit,
//     and any other regression such as published → draft is refused outright.
//   - the previously effective version is marked `retired(ROLLED_BACK)` with `supersededBy` recorded.
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

// Retire performs a manual retirement (an explicit takedown other than `validUntil` expiring).
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

// Artifact returns the rule package JSON handed to the decision service (IR form).
//
// This is the **only** artifact hand-off surface between the platform and the engine (`ARCHITECTURE.md` §2.1 "integrate only through the rule package artifact + contract").
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

// versionLess compares version numbers (both integer-style vN and semver are supported).
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
