package packages

import (
	"errors"
	"strings"
	"testing"

	"goldenrules/platform/contracts"
)

// stubVerifier 记录被调用次数，按需返回结果。
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

// approveFirstCase 把包内首条用例改成 approved（等价于业务确认）。
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

// 没有 approved 用例：门禁不需要执行器，发布照常放行。
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

// 有 approved 用例但没执行器 → fail-closed，不发布。
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
	// 关键：包状态不能被动过
	v, _ := svc.Get("pkg-withdraw-risk", "v1")
	if v.Status != StatusApproved {
		t.Fatalf("门禁拦下后状态应仍是 approved，实际 %s", v.Status)
	}
}

// 用例失败 → 阻断发布，且阻断原因里能直接看到是哪条用例、为什么。
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

// 用例全过 → 发布成功，且执行器确实被调用。
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

	// 重复发布（幂等）不应再跑一遍门禁——状态已不是 approved
	if _, err := svc.Publish("pkg-withdraw-risk", "v1", "release.A", "", ""); err != nil {
		t.Fatalf("幂等发布: %v", err)
	}
	if v.calls != 1 {
		t.Fatalf("幂等发布不应重复执行门禁，实际调用 %d 次", v.calls)
	}
}

// 预演入口只读：跑用例但不改状态，便于发布前先看红灯。
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
	// 未知版本 → NotFoundError
	var nf *NotFoundError
	if _, err := svc.Verify("pkg-withdraw-risk", "v9"); !errors.As(err, &nf) {
		t.Fatalf("未知版本应 NotFound，实际 %v", err)
	}
}

// 执行器报错（引擎挂了/超时）也算「无法证明通过」→ 阻断，不能当成通过。
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
