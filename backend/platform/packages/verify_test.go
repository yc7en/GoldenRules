package packages

import (
	"errors"
	"strings"
	"testing"

	"goldenrules/platform/contracts"
)

// stubVerifier counts how many times it is called and returns a result on demand.
type stubVerifier struct {
	calls  int
	report VerifyReport
	err    error
}

func (v *stubVerifier) Describe() string { return "stub-engine" }

func (v *stubVerifier) VerifyPackage(pkg contracts.RulePackage) (VerifyReport, error) {
	v.calls++
	return v.report, v.err
}

// approveFirstCase flips the package's first test case to approved (equivalent to business confirmation).
func approveFirstCase(t *testing.T, svc *Service, pkgID, version string) {
	t.Helper()
	err := svc.st.Update(func(r *Repo) error {
		for i := range r.Versions {
			if r.Versions[i].PackageID != pkgID || r.Versions[i].PackageVersion != version {
				continue
			}
			r.Versions[i].Package.Tests[0].Status = "approved"
			r.Versions[i].Package.Tests[0].RulesetID = "rs-amount-limit"
			r.Versions[i].ConfirmedTC = 1
			r.Versions[i].UnconfirmedTC = 0
		}
		return nil
	})
	if err != nil {
		t.Fatalf("approve case: %v", err)
	}
}

// No approved test cases: the gate needs no executor, and publish goes through as usual.
func TestPublishWithoutApprovedCasesSkipsGate(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "")
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", ""); err != nil {
		t.Fatalf("无用例时发布应成功: %v", err)
	}
}

// Approved test cases exist but there is no executor → fail-closed, no publish.
func TestPublishWithApprovedCasesRequiresVerifier(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "")
	approveFirstCase(t, svc, "pkg-withdraw-risk", "v1")
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", "")
	var se *StateError
	if !errors.As(err, &se) || se.Code != CodeVerifyUnavailable {
		t.Fatalf("应 %s，实际 %v", CodeVerifyUnavailable, err)
	}
	// Key point: the package state must not have been touched
	v, _ := svc.Get("pkg-withdraw-risk", "v1")
	if v.Status != StatusApproved {
		t.Fatalf("门禁拦下后状态应仍是 approved，实际 %s", v.Status)
	}
}

// A failing test case → publish blocked, and the blocking reason shows directly which case failed and why.
func TestPublishBlockedByFailingCase(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "")
	approveFirstCase(t, svc, "pkg-withdraw-risk", "v1")
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	svc.WithVerifier(&stubVerifier{report: VerifyReport{
		Passed: false, Ran: 1, FailedCount: 1,
		Failures: []VerifyFailure{{TestID: "T001", Code: "DECISION_MISMATCH", Message: "期望 REJECT 实际 REVIEW"}},
	}})

	_, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", "")
	var se *StateError
	if !errors.As(err, &se) || se.Code != CodeVerifyFailed {
		t.Fatalf("应 %s，实际 %v", CodeVerifyFailed, err)
	}
	if !strings.Contains(se.Message, "T001") || !strings.Contains(se.Message, "DECISION_MISMATCH") {
		t.Fatalf("阻断原因应指出用例与原因，实际：%s", se.Message)
	}
	if v, _ := svc.Get("pkg-withdraw-risk", "v1"); v.Status != StatusApproved {
		t.Fatalf("被拦下后不应改变状态，实际 %s", v.Status)
	}
}

// All test cases pass → publish succeeds, and the executor really was called.
func TestPublishRunsApprovedCases(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "")
	approveFirstCase(t, svc, "pkg-withdraw-risk", "v1")
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	v := &stubVerifier{report: VerifyReport{Passed: true, Ran: 1, PassedCount: 1, Summary: "1/1"}}
	svc.WithVerifier(v)

	pub, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", "")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Status != StatusPublished {
		t.Fatalf("状态 = %s", pub.Status)
	}
	if v.calls != 1 {
		t.Fatalf("门禁应被调用 1 次，实际 %d", v.calls)
	}

	// An idempotent re-publish must not run the gate again — the state is no longer approved
	if _, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", ""); err != nil {
		t.Fatalf("幂等发布: %v", err)
	}
	if v.calls != 1 {
		t.Fatalf("幂等发布不应重复执行门禁，实际调用 %d 次", v.calls)
	}
}

// The rehearsal entry point is read-only: it runs the test cases without changing state, so red lights can be seen before publishing.
func TestVerifyPreviewIsReadOnly(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "")
	approveFirstCase(t, svc, "pkg-withdraw-risk", "v1")
	svc.WithVerifier(&stubVerifier{report: VerifyReport{Passed: false, FailedCount: 1,
		Failures: []VerifyFailure{{TestID: "T001", Code: "OUTPUT_MISMATCH"}}}})

	rep, err := svc.Verify("pkg-withdraw-risk", "v1")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Passed {
		t.Fatal("预演应报告失败")
	}
	if v, _ := svc.Get("pkg-withdraw-risk", "v1"); v.Status != StatusDraft {
		t.Fatalf("预演不应改状态，实际 %s", v.Status)
	}
	// unknown version → NotFoundError
	var nf *NotFoundError
	if _, err := svc.Verify("pkg-withdraw-risk", "v9"); !errors.As(err, &nf) {
		t.Fatalf("未知版本应 NotFound，实际 %v", err)
	}
}

// An executor error (engine down/timeout) also counts as "cannot prove they pass" → block; never treat it as a pass.
func TestVerifierErrorBlocksPublish(t *testing.T) {
	cat := loadCatalog(t)
	svc := newSvc(t)
	mustDraft(t, svc, cat, "")
	approveFirstCase(t, svc, "pkg-withdraw-risk", "v1")
	if _, err := svc.Approve("pkg-withdraw-risk", "v1", "reviewer.B"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	svc.WithVerifier(&stubVerifier{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")})

	_, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", "")
	var se *StateError
	if !errors.As(err, &se) || se.Code != CodeVerifyUnavailable {
		t.Fatalf("执行器故障应 %s，实际 %v", CodeVerifyUnavailable, err)
	}
}
