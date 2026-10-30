package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// Ships one AI-generated, not-yet-confirmed test case: used to verify "publish warns about unconfirmed test cases" (`contracts/01` §6.1).
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

// Walks the full lifecycle "compile draft → approve → publish → supersede → roll back",
// asserting that the version count never shrinks, the artifact is deliverable, and the signature verifies.
func TestPackageLifecycleOverHTTP(t *testing.T) {
	h := newTestServer(t)
	seedAmountRuleset(t, h)

	// 1 compile draft: v1 is allocated automatically when no version is specified
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

	// 2 an unapproved version cannot be published
	rec, body = do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish", "")
	if rec.Code != http.StatusConflict || body["errorCode"] != "PKG_NOT_APPROVED" {
		t.Fatalf("未批准发布应 409 PKG_NOT_APPROVED，实际 %d %s", rec.Code, rec.Body)
	}

	// 3 there is no active version yet
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/active", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != "PKG_NOT_FOUND" {
		t.Fatalf("active 应 404，实际 %d %s", rec.Code, rec.Body)
	}

	// 4 approve → publish
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

	// 5 the active version is queryable
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/active", "")
	if rec.Code != http.StatusOK || body["packageVersion"] != "v1" {
		t.Fatalf("active = %d %s", rec.Code, rec.Body)
	}

	// 6 artifact: IR shape + signature header
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

	// 7 signature verification
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1/verify", "")
	if rec.Code != http.StatusOK || body["valid"] != true {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}

	// 8 publish the second version: the old one retires automatically, version count becomes 2
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

	// 9 a published version's content is immutable: drafting the same version again must be 409
	rec, body = do(t, h, "POST", "/api/v1/packages", strings.Replace(pkgDraftBody, `"packageId"`, `"version": "v2", "packageId"`, 1))
	if rec.Code != http.StatusConflict || body["errorCode"] != "PKG_VERSION_EXISTS" {
		t.Fatalf("重写已发布版本应 409，实际 %d %s", rec.Code, rec.Body)
	}

	// 10 rollback: switch back to v1 without producing a new version number
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

	// 11 after rollback v2 is recorded as rolled back and points at v1
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v2", "")
	if body["retiredCause"] != "ROLLED_BACK" || body["supersededBy"] != "v1" {
		t.Fatalf("v2 应记为被回滚: %s", rec.Body)
	}

	// 12 a version that does not exist
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v9", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != "PKG_NOT_FOUND" {
		t.Fatalf("未知版本应 404，实际 %d %s", rec.Code, rec.Body)
	}
}

// A compile failure is 422 + per-item diagnostics, and the whole package is not persisted (`contracts/01` §5.1 all-or-nothing).
func TestPackageCompileFailureIsAllOrNothing(t *testing.T) {
	h := newTestServer(t)
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}

	// The ruleset itself is accepted (referencing request.amount is valid), but another ruleset in the package references a missing field —
	// here we take a path other than "referencing a deprecated fact": let the package reference a ruleset that does not exist.
	rec, body := do(t, h, "POST", "/api/v1/packages",
		`{"packageId":"pkg-x","rulesetIds":["rs-amount-limit"],"changelog":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知规则集应 404，实际 %d %s", rec.Code, rec.Body)
	}
	if body["errorCode"] != "NOT_FOUND" {
		t.Fatalf("errorCode = %v", body["errorCode"])
	}

	// an empty ruleset list is a request error
	rec, body = do(t, h, "POST", "/api/v1/packages", `{"packageId":"pkg-x","rulesetIds":[]}`)
	if rec.Code != http.StatusBadRequest || body["errorCode"] != "PKG_EMPTY" {
		t.Fatalf("空包应 400 PKG_EMPTY，实际 %d %s", rec.Code, rec.Body)
	}

	// nothing was persisted
	rec, body = do(t, h, "GET", "/api/v1/packages", "")
	if rec.Code != http.StatusOK || int(body["total"].(float64)) != 0 {
		t.Fatalf("失败不应落库: %s", rec.Body)
	}
}

// Scheduled publish: a package with a future validFrom is published but not yet in effect.
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

// versionOf extracts the version summary from the publish response (the outer object is {version, warnings}).
func versionOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	v, ok := body["version"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 version: %v", body)
	}
	return v
}

// seedAmountRuleset creates one fact + one ruleset as compile input.
func seedAmountRuleset(t *testing.T, h http.Handler) {
	t.Helper()
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, "PUT", "/api/v1/rulesets/rs-amount-limit", rulesetWithPendingTest); rec.Code != http.StatusOK {
		t.Fatalf("put ruleset = %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- test case gate (F-PKG06)

// A ruleset carrying one approved test case: the gate must actually run it (or explicitly report "nobody can run it").
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

// Without a gate executor, a package carrying approved test cases must not be publishable (fail-closed).
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
	// the state did not move
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1", "")
	if rec.Code != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("被拦下后应仍是 approved，实际 %d %s", rec.Code, rec.Body)
	}
}

// Rehearsal entry point: also 503 when there is no executor, and it does not change state.
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

// With only unconfirmed test cases, rehearsal returns "nothing to run" rather than an error — the two situations must not look alike.
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

// Rehearsing an unknown version → 404.
func TestTestcasePreviewUnknownVersion(t *testing.T) {
	h := newTestServer(t)
	rec, body := do(t, h, "GET", "/api/v1/packages/pkg-nope/versions/v1/testcases", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != "PKG_NOT_FOUND" {
		t.Fatalf("未知包应 404，实际 %d %s", rec.Code, rec.Body)
	}
}
