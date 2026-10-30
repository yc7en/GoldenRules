package packages

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"goldenrules/platform/contracts"
)

// VerifyFailure is the failure reason of a single test case (isomorphic to the engine-side `TestFailure`).
type VerifyFailure struct {
	TestID  string `json:"testId"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// VerifyReport is the gate execution result.
type VerifyReport struct {
	Passed      bool            `json:"passed"`
	Summary     string          `json:"summary"`
	Total       int             `json:"total"`
	Ran         int             `json:"ran"`
	PassedCount int             `json:"passedCount"`
	FailedCount int             `json:"failedCount"`
	Skipped     int             `json:"skipped"`
	Failures    []VerifyFailure `json:"failures"`
	// Backend self-describes "who ran it" (engine address/version), so the audit can be reconciled.
	Backend string `json:"backend"`
}

// TestCaseVerifier runs the approved test cases inside a rule package.
//
// Why an interface rather than an in-platform implementation: the VM exists only inside the engine. A second, "approximate executor" on the platform side
// would drift from online behavior sooner or later — using a drifted executor as the publish gate is more dangerous than having no gate at all.
type TestCaseVerifier interface {
	VerifyPackage(pkg contracts.RulePackage) (VerifyReport, error)
	Describe() string
}

// HTTPTestCaseVerifier calls the engine's `/v1/admin/verify` (the default implementation for co-located deployments).
type HTTPTestCaseVerifier struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPTestCaseVerifier builds the engine gate client.
//
// The gate is a synchronous blocking step, so the timeout must be short: hanging the publish is worse than an error — the releaser would assume it is "stuck" and force it through.
func NewHTTPTestCaseVerifier(baseURL, token string) *HTTPTestCaseVerifier {
	return &HTTPTestCaseVerifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Describe self-describes the backend (written into the audit and the publish record).
func (v *HTTPTestCaseVerifier) Describe() string {
	if v == nil || v.baseURL == "" {
		return ""
	}
	return "engine:" + v.baseURL + " /v1/admin/verify"
}

// VerifyPackage sends the candidate package to the engine for one run.
func (v *HTTPTestCaseVerifier) VerifyPackage(pkg contracts.RulePackage) (VerifyReport, error) {
	var out VerifyReport
	if v.baseURL == "" {
		return out, fmt.Errorf("未配置决策服务地址，无法执行用例门禁")
	}
	body, err := json.Marshal(map[string]any{"package": pkg})
	if err != nil {
		return out, fmt.Errorf("序列化规则包失败: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, v.baseURL+"/v1/admin/verify", bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("构造门禁请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if v.token != "" {
		req.Header.Set("X-Reload-Token", v.token)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		// No connection to the engine = cannot prove the test cases pass. This must be propagated so the caller can fail-closed.
		return out, fmt.Errorf("调用决策服务门禁失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var ec contracts.ErrorContract
		if derr := json.NewDecoder(resp.Body).Decode(&ec); derr == nil && ec.ErrorCode != "" {
			return out, fmt.Errorf("门禁返回 %d %s: %s", resp.StatusCode, ec.ErrorCode, ec.Message)
		}
		return out, fmt.Errorf("门禁返回 HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("解析门禁响应失败: %w", err)
	}
	out.Backend = v.Describe()
	return out, nil
}

// ---------------------------------------------------------------------------
// Publish gate wiring (F-PKG06 / `contracts/01` §6.1)

// Verify rehearses the approved test cases of a version (read-only, no state change).
//
// Purpose: the releaser runs it once before actually pressing "publish", so failing cases are laid out in front of them;
// it also lets CI stop a bad package early in the pipeline instead of discovering it at the moment of the publish action.
func (s *Service) Verify(packageID, version string) (VerifyReport, error) {
	v, ok := s.Get(packageID, version)
	if !ok {
		return VerifyReport{}, &NotFoundError{PackageID: packageID, Version: version}
	}
	return s.verifyVersion(v)
}

// runPublishGate is the pre-publish gate: any failing approved test case → block.
//
// Three "do not block" cases must be told apart (confusing them would let things through by mistake):
//   - the package has no approved test cases: nothing to verify, so let it pass but report skipped;
//   - the version does not exist or is in the wrong state: leave the error to Publish's state machine, do not jump the gun here;
//   - the version is already published and effective (idempotent re-publish): no need to rerun.
func (s *Service) runPublishGate(packageID, version string) error {
	v, ok := s.Get(packageID, version)
	if !ok || v.Status != StatusApproved {
		return nil
	}
	rep, err := s.verifyVersion(v)
	if err != nil {
		return err
	}
	if !rep.Passed {
		return &StateError{Code: CodeVerifyFailed, Message: gateMessage(rep)}
	}
	return nil
}

// verifyVersion finds the approved test cases and hands them to the executor.
func (s *Service) verifyVersion(v Version) (VerifyReport, error) {
	total := len(v.Package.Tests)
	approved := 0
	for _, tc := range v.Package.Tests {
		if tc.Status == "approved" {
			approved++
		}
	}
	if approved == 0 {
		return VerifyReport{
			Passed:  true,
			Summary: fmt.Sprintf("包内没有 approved 用例（共 %d 条未确认），无需执行", total),
			Total:   total,
			Skipped: total,
		}, nil
	}
	if s.verifier == nil {
		// fail-closed: approved test cases are claimed, yet nothing can prove they pass → do not let it through.
		return VerifyReport{}, &StateError{
			Code: CodeVerifyUnavailable,
			Message: fmt.Sprintf("包内有 %d 条 approved 用例待执行，但平台未配置门禁执行器（--engine-url）",
				approved),
		}
	}
	rep, err := s.verifier.VerifyPackage(v.Package)
	if err != nil {
		return VerifyReport{}, &StateError{
			Code:    CodeVerifyUnavailable,
			Message: "用例门禁执行失败：" + err.Error(),
		}
	}
	return rep, nil
}

// gateMessage condenses the failure list into one readable blocking reason (at most 3 items, to avoid a log explosion).
func gateMessage(rep VerifyReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d 条 approved 用例未通过：", rep.FailedCount)
	for i, f := range rep.Failures {
		if i >= 3 {
			fmt.Fprintf(&b, "；等共 %d 条", len(rep.Failures))
			break
		}
		if i > 0 {
			b.WriteString("；")
		}
		fmt.Fprintf(&b, "%s %s %s", f.TestID, f.Code, f.Message)
	}
	return b.String()
}
