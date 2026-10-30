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

// VerifyFailure 单条用例的失败原因（与引擎侧 `TestFailure` 同构）。
type VerifyFailure struct {
	TestID  string `json:"testId"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// VerifyReport 门禁执行结果。
type VerifyReport struct {
	Passed      bool            `json:"passed"`
	Summary     string          `json:"summary"`
	Total       int             `json:"total"`
	Ran         int             `json:"ran"`
	PassedCount int             `json:"passedCount"`
	FailedCount int             `json:"failedCount"`
	Skipped     int             `json:"skipped"`
	Failures    []VerifyFailure `json:"failures"`
	// Backend 自述「谁跑的」（引擎地址/版本），审计里要能对上。
	Backend string `json:"backend"`
}

// TestCaseVerifier 执行规则包内已批准用例。
//
// 为什么是接口而不是平台内实现：VM 只有引擎那一份。平台侧再写一个「近似执行器」，
// 迟早与线上行为漂移——用漂移的执行器当发布门禁，比没有门禁更危险。
type TestCaseVerifier interface {
	VerifyPackage(pkg contracts.RulePackage) (VerifyReport, error)
	Describe() string
}

// HTTPTestCaseVerifier 调引擎的 `/v1/admin/verify`（同机部署时的默认实现）。
type HTTPTestCaseVerifier struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPTestCaseVerifier 构造引擎门禁客户端。
//
// 门禁是同步阻断动作，超时要短：卡住发布比报错更糟——发布人会以为「卡死了」然后强推。
func NewHTTPTestCaseVerifier(baseURL, token string) *HTTPTestCaseVerifier {
	return &HTTPTestCaseVerifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Describe 自述后端（写进审计与发布记录）。
func (v *HTTPTestCaseVerifier) Describe() string {
	if v == nil || v.baseURL == "" {
		return ""
	}
	return "engine:" + v.baseURL + " /v1/admin/verify"
}

// VerifyPackage 把候选包发给引擎跑一遍。
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
		// 连不上引擎 = 无法证明用例通过。这条必须往上抛，让调用方 fail-closed。
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
// 发布门禁接线（F-PKG06 / `contracts/01` §6.1）

// Verify 预演某版本的 approved 用例（只读，不改状态）。
//
// 用途：发布人在真正点「发布」前先跑一次，把失败用例摆在面前；
// 也让 CI 能在流程早期拦下坏包，而不是等到发布动作那一刻才发现。
func (s *Service) Verify(packageID, version string) (VerifyReport, error) {
	v, ok := s.Get(packageID, version)
	if !ok {
		return VerifyReport{}, &NotFoundError{PackageID: packageID, Version: version}
	}
	return s.verifyVersion(v)
}

// runPublishGate 发布前置门禁：任一 approved 用例失败 → 阻断。
//
// 三种「不阻断」的情形要分清（混淆它们会造成误放行）：
//   - 包里没有 approved 用例：没东西可验，放行但报告 skipped；
//   - 版本不存在或状态不对：交给 Publish 的状态机报错，这里不抢答；
//   - 版本已发布且已生效（重复发布幂等）：不必重跑。
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

// verifyVersion 找出 approved 用例并交给执行器。
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
		// fail-closed：声称有已批准用例，却没有任何东西能证明它们通过 → 不放行。
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

// gateMessage 把失败清单压成一句可读的阻断原因（最多列 3 条，避免日志爆炸）。
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
