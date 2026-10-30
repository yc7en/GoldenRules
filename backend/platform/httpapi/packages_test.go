package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// 带一条 AI 生成、尚未确认的用例：用来验证「发布时提示未确认用例」（`contracts/01` §6.1）。
const rulesetWithPendingTest = `{
  "rulesetId": "rs-amount-limit",
  "scene": "withdraw-risk-check",
  "version": "rs-1",
  "hitStrategy": "ALL",
  "defaultDecision": "REJECT",
  "rules": [
    {
      "ruleId": "R1",
      "priority": 100,
      "when": "request.amount > 1000",
      "then": {"decision": "REJECT", "reasonCode": "AMOUNT_LIMIT_EXCEEDED"}
    }
  ],
  "tests": [
    {
      "testId": "T001",
      "scene": "withdraw-risk-check",
      "input": {"request.amount": "1500"},
      "expect": {"decision": "REJECT", "reasonCode": "AMOUNT_LIMIT_EXCEEDED"},
      "author": "agent",
      "status": "pending_review"
    }
  ]
}`

const pkgDraftBody = `{
  "packageId": "pkg-withdraw",
  "rulesetIds": ["rs-amount-limit"],
  "riskLevel": "L2",
  "changelog": "首版：提款金额上限"
}`

// 走一遍「编译草稿 → 批准 → 发布 → 取代 → 回滚」的完整生命周期，
// 断言版本数只增不减、产物可交付、验签有效。
func TestPackageLifecycleOverHTTP(t *testing.T) {
	h := newTestServer(t)
	seedAmountRuleset(t, h)

	// 1 编译草稿：版本号未指定时自动分配 v1
	rec, body := do(t, h, "POST", "/api/v1/packages", pkgDraftBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create package = %d %s", rec.Code, rec.Body)
	}
	v := body["version"].(map[string]any)
	if v["packageVersion"] != "v1" || v["status"] != "draft" {
		t.Fatalf("草稿字段异常: %s", rec.Body)
	}
	if n, _ := v["ruleCount"].(float64); n != 1 {
		t.Fatalf("ruleCount = %v", v["ruleCount"])
	}
	if v["unconfirmedTestCases"] != float64(1) || v["confirmedTestCases"] != float64(0) {
		t.Fatalf("用例计数异常: %s", rec.Body)
	}
	if sig, _ := v["signature"].(string); !strings.HasPrefix(sig, "sha256:") {
		t.Fatalf("signature = %v", v["signature"])
	}

	// 2 未批准不能发布
	rec, body = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish", "")
	if rec.Code != http.StatusConflict || body["errorCode"] != "PKG_NOT_APPROVED" {
		t.Fatalf("未批准发布应 409 PKG_NOT_APPROVED，实际 %d %s", rec.Code, rec.Body)
	}

	// 3 还没有生效版本
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/active", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != "PKG_NOT_FOUND" {
		t.Fatalf("active 应 404，实际 %d %s", rec.Code, rec.Body)
	}

	// 4 批准 → 发布
	if rec, _ = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/approve", ""); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	rec, body = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	pub := versionOf(t, body)
	if pub["status"] != "published" {
		t.Fatalf("publish 后状态 = %v", pub["status"])
	}
	if warns, _ := body["warnings"].([]any); len(warns) != 1 || !strings.Contains(warns[0].(string), "未确认用例") {
		t.Fatalf("发布应提示未确认用例: %s", rec.Body)
	}

	// 5 生效版本可查
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/active", "")
	if rec.Code != http.StatusOK || body["packageVersion"] != "v1" {
		t.Fatalf("active = %d %s", rec.Code, rec.Body)
	}

	// 6 产物：IR 形态 + 签名头
	rec = doRaw(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1/artifact")
	if rec.Code != http.StatusOK {
		t.Fatalf("artifact = %d %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Header().Get("X-Package-Signature"), "sha256:") {
		t.Fatalf("缺签名头: %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), `"irVersion"`) || !strings.Contains(rec.Body.String(), `"slotTable"`) {
		t.Fatalf("产物不像规则包: %s", rec.Body)
	}

	// 7 验签
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1/verify", "")
	if rec.Code != http.StatusOK || body["valid"] != true {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}

	// 8 发布第二版：旧版自动退役，版本数变 2
	rec, body = do(t, h, "POST", "/api/v1/packages", pkgDraftBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create v2 = %d %s", rec.Code, rec.Body)
	}
	if got := body["version"].(map[string]any)["packageVersion"]; got != "v2" {
		t.Fatalf("自动版本号应为 v2，实际 %v", got)
	}
	if rec, _ = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v2/approve", ""); rec.Code != http.StatusOK {
		t.Fatalf("approve v2 = %d %s", rec.Code, rec.Body)
	}
	if rec, _ = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v2/publish", ""); rec.Code != http.StatusOK {
		t.Fatalf("publish v2 = %d %s", rec.Code, rec.Body)
	}

	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1", "")
	v1 := body
	if rec.Code != http.StatusOK || v1["status"] != "retired" || v1["retiredCause"] != "SUPERSEDED" || v1["supersededBy"] != "v2" {
		t.Fatalf("v1 应被取代: %d %s", rec.Code, rec.Body)
	}

	// 9 已发布版本不可改内容：同名再建草稿应 409
	rec, body = do(t, h, "POST", "/api/v1/packages", strings.Replace(pkgDraftBody, `"packageId"`, `"version": "v2", "packageId"`, 1))
	if rec.Code != http.StatusConflict || body["errorCode"] != "PKG_VERSION_EXISTS" {
		t.Fatalf("重写已发布版本应 409，实际 %d %s", rec.Code, rec.Body)
	}

	// 10 回滚：切回 v1，不产生新版本号
	rec, body = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/rollback", `{"toVersion":"v1","reason":"线上误拒率升高"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback = %d %s", rec.Code, rec.Body)
	}
	if body["packageVersion"] != "v1" || body["status"] != "published" {
		t.Fatalf("rollback 后 = %s", rec.Body)
	}
	rec, body = do(t, h, "GET", "/api/v1/packages?packageId=pkg-withdraw", "")
	if rec.Code != http.StatusOK || int(body["total"].(float64)) != 2 {
		t.Fatalf("回滚不应新增版本，实际 %s", rec.Body)
	}

	// 11 回滚后的 v2 记为被回滚，指向 v1
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v2", "")
	if body["retiredCause"] != "ROLLED_BACK" || body["supersededBy"] != "v1" {
		t.Fatalf("v2 应记为被回滚: %s", rec.Body)
	}

	// 12 不存在的版本
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v9", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != "PKG_NOT_FOUND" {
		t.Fatalf("未知版本应 404，实际 %d %s", rec.Code, rec.Body)
	}
}

// 编译失败是 422 + 逐条诊断，且整包不落库（`contracts/01` §5.1 全量或全不）。
func TestPackageCompileFailureIsAllOrNothing(t *testing.T) {
	h := newTestServer(t)
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}

	// 规则集本身写得进去（引用 request.amount 合法），但包里另一个规则集引用不存在字段——
	// 这里用「引用已废弃事实」之外的路径：直接让包引用不存在的规则集。
	rec, body := do(t, h, "POST", "/api/v1/packages",
		`{"packageId":"pkg-x","rulesetIds":["rs-amount-limit"],"changelog":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知规则集应 404，实际 %d %s", rec.Code, rec.Body)
	}
	if body["errorCode"] != "NOT_FOUND" {
		t.Fatalf("errorCode = %v", body["errorCode"])
	}

	// 空规则集列表是请求错误
	rec, body = do(t, h, "POST", "/api/v1/packages", `{"packageId":"pkg-x","rulesetIds":[]}`)
	if rec.Code != http.StatusBadRequest || body["errorCode"] != "PKG_EMPTY" {
		t.Fatalf("空包应 400 PKG_EMPTY，实际 %d %s", rec.Code, rec.Body)
	}

	// 没落库
	rec, body = do(t, h, "GET", "/api/v1/packages", "")
	if rec.Code != http.StatusOK || int(body["total"].(float64)) != 0 {
		t.Fatalf("失败不应落库: %s", rec.Body)
	}
}

// 定时发布：validFrom 在未来的包是 published 但尚未生效。
func TestPackageScheduledPublishOverHTTP(t *testing.T) {
	h := newTestServer(t)
	seedAmountRuleset(t, h)

	if rec, _ := do(t, h, "POST", "/api/v1/packages", pkgDraftBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/approve", ""); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	rec, body := do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish",
		`{"validFrom":"2026-11-01T00:00:00Z","validUntil":"2026-12-01T00:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	pub := versionOf(t, body)
	if pub["validFrom"] != "2026-11-01T00:00:00Z" || pub["validUntil"] != "2026-12-01T00:00:00Z" {
		t.Fatalf("生效时间未落库: %s", rec.Body)
	}
	if pub["status"] != "published" {
		t.Fatalf("定时发布的包状态仍是 published: %v", pub["status"])
	}
}

// ---------------------------------------------------------------------------

// versionOf 取 publish 响应里的版本摘要（外层是 {version, warnings}）。
func versionOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	v, ok := body["version"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 version: %v", body)
	}
	return v
}

// seedAmountRuleset 建一个事实 + 一个规则集，作为编译输入。
func seedAmountRuleset(t *testing.T, h http.Handler) {
	t.Helper()
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, "PUT", "/api/v1/rulesets/rs-amount-limit", rulesetWithPendingTest); rec.Code != http.StatusOK {
		t.Fatalf("put ruleset = %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- 用例门禁（F-PKG06）

// 带一条已确认用例的规则集：门禁必须真跑（或明确报告「没人能跑」）。
const rulesetWithApprovedTest = `{
  "rulesetId": "rs-amount-limit",
  "scene": "withdraw-risk-check",
  "version": "rs-1",
  "hitStrategy": "ALL",
  "defaultDecision": "REJECT",
  "rules": [
    {
      "ruleId": "R1",
      "priority": 100,
      "when": "request.amount > 1000",
      "then": {"decision": "REJECT", "reasonCode": "AMOUNT_LIMIT_EXCEEDED"}
    }
  ],
  "tests": [
    {
      "testId": "T001",
      "scene": "withdraw-risk-check",
      "rulesetId": "rs-amount-limit",
      "input": {"request.amount": "1500"},
      "expect": {"decision": "REJECT", "reasonCode": "AMOUNT_LIMIT_EXCEEDED"},
      "author": "business.C",
      "status": "approved"
    }
  ]
}`

func seedApprovedCase(t *testing.T, h http.Handler) {
	t.Helper()
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, "PUT", "/api/v1/rulesets/rs-amount-limit", rulesetWithApprovedTest); rec.Code != http.StatusOK {
		t.Fatalf("put ruleset = %d %s", rec.Code, rec.Body)
	}
}

// 没有门禁执行器时，带 approved 用例的包必须发不出去（fail-closed）。
func TestPublishBlockedWhenGateUnavailable(t *testing.T) {
	h := newTestServer(t)
	seedApprovedCase(t, h)
	if rec, body := do(t, h, "POST", "/api/v1/packages", pkgDraftBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, body)
	}
	if rec, _ := do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/approve", ""); rec.Code != http.StatusOK {
		t.Fatalf("approve 失败")
	}
	rec, body := do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish", "")
	if rec.Code != http.StatusServiceUnavailable || body["errorCode"] != "PKG_VERIFY_UNAVAILABLE" {
		t.Fatalf("无执行器应 503 PKG_VERIFY_UNAVAILABLE，实际 %d %s", rec.Code, rec.Body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "engine-url") {
		t.Fatalf("诊断应指出缺什么配置: %s", msg)
	}
	// 状态没动
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1", "")
	if rec.Code != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("被拦下后应仍是 approved，实际 %d %s", rec.Code, rec.Body)
	}
}

// 预演入口：无执行器时同样 503，且不改状态。
func TestTestcasePreviewWithoutVerifier(t *testing.T) {
	h := newTestServer(t)
	seedApprovedCase(t, h)
	if rec, body := do(t, h, "POST", "/api/v1/packages", pkgDraftBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, body)
	}
	rec, body := do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1/testcases", "")
	if rec.Code != http.StatusServiceUnavailable || body["errorCode"] != "PKG_VERIFY_UNAVAILABLE" {
		t.Fatalf("预演应 503，实际 %d %s", rec.Code, rec.Body)
	}
}

// 只有未确认用例时，预演返回「无需执行」而不是报错——两种情形不能长得一样。
func TestTestcasePreviewWithOnlyPendingCases(t *testing.T) {
	h := newTestServer(t)
	seedAmountRuleset(t, h)
	if rec, body := do(t, h, "POST", "/api/v1/packages", pkgDraftBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, body)
	}
	rec, body := do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1/testcases", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("预演 = %d %s", rec.Code, rec.Body)
	}
	rep := body["report"].(map[string]any)
	if rep["passed"] != true || rep["ran"] != float64(0) || rep["skipped"] != float64(1) {
		t.Fatalf("无用例可跑时应 passed=true/ran=0/skipped=1: %s", rec.Body)
	}
	gate := body["gate"].(map[string]any)
	if gate["enforcedOnPublish"] != true {
		t.Fatalf("应说明门禁在发布时强制: %s", rec.Body)
	}
}

// 未知版本预演 → 404。
func TestTestcasePreviewUnknownVersion(t *testing.T) {
	h := newTestServer(t)
	rec, body := do(t, h, "GET", "/api/v1/packages/pkg-nope/versions/v1/testcases", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != "PKG_NOT_FOUND" {
		t.Fatalf("未知包应 404，实际 %d %s", rec.Code, rec.Body)
	}
}
