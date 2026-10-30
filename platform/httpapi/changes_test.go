package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// 治理环境：发布必须挂变更单，直接 approve 版本被关闭。
func TestChangeGovernanceOverHTTP(t *testing.T) {
	h := newGovTestServer(t)
	seedAmountRuleset(t, h)
	createDraft(t, h, "v1", "L3")

	// 1 直接批版本被关闭（绕过审批矩阵）
	rec, body := do(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/approve", "")
	if rec.Code != http.StatusConflict || body["errorCode"] != "GOV_CHANGE_REQUIRED" {
		t.Fatalf("直接批版本应 409 GOV_CHANGE_REQUIRED，实际 %d %s", rec.Code, rec.Body)
	}

	// 2 agent 提 L4 被拒（A-8 硬规则）
	createDraft(t, h, "v9", "L4")
	rec, body = doAs(t, h, "POST", "/api/v1/changes",
		`{"packageId":"pkg-withdraw","packageVersion":"v9","riskLevel":"L4","reason":"制裁名单","submitterType":"agent"}`,
		"agent:rule-writer", "agent")
	if rec.Code != http.StatusForbidden || body["errorCode"] != "GOV_MATRIX_REJECTED" {
		t.Fatalf("agent 提 L4 应 403，实际 %d %s", rec.Code, rec.Body)
	}

	// 3 工程师提 L3：需要 2 人
	rec, body = doAs(t, h, "POST", "/api/v1/changes",
		`{"packageId":"pkg-withdraw","packageVersion":"v1","riskLevel":"L3","changeType":"threshold","reason":"调整限额"}`,
		"engineer.A", "engineer")
	if rec.Code != http.StatusCreated {
		t.Fatalf("提变更单 = %d %s", rec.Code, rec.Body)
	}
	changeID := body["changeId"].(string)
	if n := len(body["slots"].([]any)); n != 2 {
		t.Fatalf("L3 应有 2 个审批位: %s", rec.Body)
	}

	// 4 提交者自审被拒（L3 不允许）
	rec, body = doAs(t, h, "POST", "/api/v1/changes/"+changeID+"/approve",
		`{"role":"engineer","comment":"自己批"}`, "engineer.A", "engineer")
	if rec.Code != http.StatusForbidden || body["errorCode"] != "GOV_SELF_APPROVAL" {
		t.Fatalf("自审应 403，实际 %d %s", rec.Code, rec.Body)
	}

	// 5 角色不在待批位
	rec, body = doAs(t, h, "POST", "/api/v1/changes/"+changeID+"/approve",
		`{"role":"owner","comment":"越位"}`, "owner.U", "engineer")
	if rec.Code != http.StatusConflict || body["errorCode"] != "GOV_ROLE_NOT_REQUIRED" {
		t.Fatalf("越位审批应 409，实际 %d %s", rec.Code, rec.Body)
	}

	// 6 两位审批人批满 → 版本推进到 approved
	if rec, _ = doAs(t, h, "POST", "/api/v1/changes/"+changeID+"/approve",
		`{"role":"engineer","comment":"ok"}`, "engineer.Z", "engineer"); rec.Code != http.StatusOK {
		t.Fatalf("工程师审批 = %d %s", rec.Code, rec.Body)
	}
	rec, body = doAs(t, h, "POST", "/api/v1/changes/"+changeID+"/approve",
		`{"role":"risk","comment":"风控同意"}`, "risk.W", "engineer")
	if rec.Code != http.StatusOK {
		t.Fatalf("风控审批 = %d %s", rec.Code, rec.Body)
	}
	if body["remaining"] != float64(0) || body["change"].(map[string]any)["status"] != "approved" {
		t.Fatalf("满票应 approved: %s", rec.Body)
	}
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/versions/v1", "")
	if body["status"] != "approved" {
		t.Fatalf("版本应推进到 approved: %s", rec.Body)
	}

	// 7 不挂变更单发布 → 409
	rec, body = doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish", "", "release.R", "engineer")
	if rec.Code != http.StatusConflict || body["errorCode"] != "GOV_CHANGE_REQUIRED" {
		t.Fatalf("无变更单发布应 409，实际 %d %s", rec.Code, rec.Body)
	}

	// 8 审批人不能发布（职责分离）
	rec, body = doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish",
		`{"changeId":"`+changeID+`"}`, "engineer.Z", "engineer")
	if rec.Code != http.StatusForbidden || body["errorCode"] != "GOV_SOD_APPROVE_PUBLISH" {
		t.Fatalf("审批人发布应 403，实际 %d %s", rec.Code, rec.Body)
	}

	// 9 提交者不能发布
	rec, body = doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish",
		`{"changeId":"`+changeID+`"}`, "engineer.A", "engineer")
	if rec.Code != http.StatusForbidden || body["errorCode"] != "GOV_SOD_SUBMIT_PUBLISH" {
		t.Fatalf("提交者发布应 403，实际 %d %s", rec.Code, rec.Body)
	}

	// 10 发布人发布成功，变更单收尾为 published，且留痕
	rec, body = doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish",
		`{"changeId":"`+changeID+`"}`, "release.R", "engineer")
	if rec.Code != http.StatusOK {
		t.Fatalf("发布 = %d %s", rec.Code, rec.Body)
	}
	rec, body = do(t, h, "GET", "/api/v1/changes/"+changeID, "")
	c := body
	if c["status"] != "published" || c["publishedBy"] != "release.R" {
		t.Fatalf("变更单收尾异常: %s", rec.Body)
	}

	rec, body = do(t, h, "GET", "/api/v1/audit?changeId="+changeID, "")
	actions := map[string]int{}
	for _, it := range body["items"].([]any) {
		actions[it.(map[string]any)["action"].(string)]++
	}
	for _, want := range []string{"CHANGE_SUBMITTED", "CHANGE_APPROVED", "PACKAGE_PUBLISHED"} {
		if actions[want] == 0 {
			t.Fatalf("缺审计动作 %s: %s", want, rec.Body)
		}
	}
	if actions["CHANGE_APPROVED"] != 2 {
		t.Fatalf("应有 2 条审批审计，实际 %d", actions["CHANGE_APPROVED"])
	}
}

// 灰度放量可调；L4 不许一步到 100%。
func TestGrayReleaseOverHTTP(t *testing.T) {
	h := newGovTestServer(t)
	seedAmountRuleset(t, h)
	createDraft(t, h, "v1", "L4")
	changeID := submitChange(t, h, "v1", "L4", "engineer.A", "engineer")
	approveAs(t, h, changeID, "engineer", "engineer.Z")
	approveAs(t, h, changeID, "risk", "risk.W")
	approveAs(t, h, changeID, "owner", "owner.U")
	if rec, _ := doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish",
		`{"changeId":"`+changeID+`"}`, "release.R", "engineer"); rec.Code != http.StatusOK {
		t.Fatalf("发布 = %d %s", rec.Code, rec.Body)
	}

	rec, body := doAs(t, h, "PUT", "/api/v1/packages/pkg-withdraw/gray",
		`{"version":"v1","mode":"percentage","percentage":100,"reason":"直接全量"}`, "release.R", "engineer")
	if rec.Code != http.StatusConflict || body["errorCode"] != "GOV_GRAY_STEP_TOO_LARGE" {
		t.Fatalf("L4 一步到 100%% 应 409，实际 %d %s", rec.Code, rec.Body)
	}

	rec, body = doAs(t, h, "PUT", "/api/v1/packages/pkg-withdraw/gray",
		`{"version":"v1","mode":"percentage","percentage":5,"reason":"首档"}`, "release.R", "engineer")
	if rec.Code != http.StatusOK {
		t.Fatalf("首档 5%% = %d %s", rec.Code, rec.Body)
	}
	if warns, _ := body["warnings"].([]any); len(warns) != 1 || !strings.Contains(warns[0].(string), "影子") {
		t.Fatalf("L4 直接放量应提示先影子: %s", rec.Body)
	}
	for _, p := range []string{"50", "100"} {
		if rec, _ = doAs(t, h, "PUT", "/api/v1/packages/pkg-withdraw/gray",
			`{"version":"v1","mode":"percentage","percentage":`+p+`}`, "release.R", "engineer"); rec.Code != http.StatusOK {
			t.Fatalf("放量到 %s%% = %d %s", p, rec.Code, rec.Body)
		}
	}
	rec, body = do(t, h, "GET", "/api/v1/packages/pkg-withdraw/gray", "")
	g := body
	if g["percentage"] != float64(100) || g["peakPercentage"] != float64(100) {
		t.Fatalf("灰度终态异常: %s", rec.Body)
	}
	if n := len(g["history"].([]any)); n != 3 {
		t.Fatalf("放量历史应有 3 步，实际 %d", n)
	}

	// 形态约束：shadow 不能带比例
	rec, body = doAs(t, h, "PUT", "/api/v1/packages/pkg-withdraw/gray",
		`{"version":"v1","mode":"shadow","percentage":30}`, "release.R", "engineer")
	if rec.Code != http.StatusUnprocessableEntity || body["errorCode"] != "GOV_GRAY_INVALID" {
		t.Fatalf("shadow 带比例应 422，实际 %d %s", rec.Code, rec.Body)
	}
}

// 扁平回滚入口：原因必填、agent 不得回滚、全过程留痕（PLAN B9 验收）。
func TestRollbackOverHTTP(t *testing.T) {
	h := newGovTestServer(t)
	seedAmountRuleset(t, h)
	createDraft(t, h, "v1", "L1")
	c1 := submitChange(t, h, "v1", "L1", "engineer.A", "engineer")
	approveAs(t, h, c1, "engineer", "engineer.Z")
	if rec, _ := doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v1/publish",
		`{"changeId":"`+c1+`"}`, "release.R", "engineer"); rec.Code != http.StatusOK {
		t.Fatalf("发布 v1 = %d %s", rec.Code, rec.Body)
	}

	createDraft(t, h, "v2", "L1")
	c2 := submitChange(t, h, "v2", "L1", "engineer.A", "engineer")
	approveAs(t, h, c2, "engineer", "engineer.Z")
	if rec, _ := doAs(t, h, "POST", "/api/v1/packages/pkg-withdraw/versions/v2/publish",
		`{"changeId":"`+c2+`"}`, "release.R", "engineer"); rec.Code != http.StatusOK {
		t.Fatalf("发布 v2 = %d %s", rec.Code, rec.Body)
	}

	// 原因必填
	rec, body := doAs(t, h, "POST", "/api/v1/rollback", `{"packageId":"pkg-withdraw","toVersion":"v1"}`, "release.R", "engineer")
	if rec.Code != http.StatusBadRequest || body["errorCode"] != "GOV_INVALID_ARGUMENT" {
		t.Fatalf("无原因回滚应 400，实际 %d %s", rec.Code, rec.Body)
	}
	// agent 不能回滚
	rec, body = doAs(t, h, "POST", "/api/v1/rollback",
		`{"packageId":"pkg-withdraw","toVersion":"v1","reason":"机器自己回滚"}`,
		"agent:ops", "agent")
	if rec.Code != http.StatusForbidden || body["errorCode"] != "GOV_AGENT_CANNOT_PUBLISH" {
		t.Fatalf("agent 回滚应 403，实际 %d %s", rec.Code, rec.Body)
	}

	rec, body = doAs(t, h, "POST", "/api/v1/rollback",
		`{"packageId":"pkg-withdraw","toVersion":"v1","reason":"线上误拒率升高"}`,
		"release.R", "engineer")
	if rec.Code != http.StatusOK {
		t.Fatalf("回滚 = %d %s", rec.Code, rec.Body)
	}
	if body["packageVersion"] != "v1" || body["status"] != "published" {
		t.Fatalf("回滚后应有 v1 生效: %s", rec.Body)
	}

	// 版本数不变 + 审计留痕
	rec, body = do(t, h, "GET", "/api/v1/packages?packageId=pkg-withdraw", "")
	if int(body["total"].(float64)) != 2 {
		t.Fatalf("回滚不应新增版本: %s", rec.Body)
	}
	rec, body = do(t, h, "GET", "/api/v1/audit?packageId=pkg-withdraw", "")
	var rb map[string]any
	for _, it := range body["items"].([]any) {
		e := it.(map[string]any)
		if e["action"] == "PACKAGE_ROLLED_BACK" {
			rb = e
		}
	}
	if rb == nil {
		t.Fatalf("缺回滚审计: %s", rec.Body)
	}
	if rb["from"] != "v2" || rb["to"] != "v1" || rb["reason"] != "线上误拒率升高" || rb["actor"] != "release.R" {
		t.Fatalf("回滚审计字段不全: %v", rb)
	}
}

// 矩阵导出 12 格，agent×L4 为禁止格。
func TestApprovalMatrixOverHTTP(t *testing.T) {
	h := newGovTestServer(t)
	rec, body := do(t, h, "GET", "/api/v1/approval-matrix", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("matrix = %d %s", rec.Code, rec.Body)
	}
	cells := body["cells"].([]any)
	if len(cells) != 12 {
		t.Fatalf("应有 12 格，实际 %d", len(cells))
	}
	for _, it := range cells {
		c := it.(map[string]any)
		if c["submitterType"] == "agent" && c["riskLevel"] == "L4" && c["forbidden"] != true {
			t.Fatalf("agent×L4 应禁止: %v", c)
		}
	}
}

// ---------------------------------------------------------------------------

// createDraft 建一个草稿版本。
func createDraft(t *testing.T, h http.Handler, version, risk string) {
	t.Helper()
	body := `{"packageId":"pkg-withdraw","rulesetIds":["rs-amount-limit"],"riskLevel":"` + risk + `","changelog":"测试"}`
	if version != "" {
		body = `{"version":"` + version + `",` + strings.TrimPrefix(body, `{`)
	}
	rec, resp := do(t, h, "POST", "/api/v1/packages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft %s = %d %s", version, rec.Code, rec.Body)
	}
	_ = resp
}

// submitChange 提交变更单并返回 changeId。
func submitChange(t *testing.T, h http.Handler, version, risk, actor, actorType string) string {
	t.Helper()
	body := `{"packageId":"pkg-withdraw","packageVersion":"` + version + `","riskLevel":"` + risk +
		`","changeType":"logic","reason":"测试变更"}`
	rec, resp := doAs(t, h, "POST", "/api/v1/changes", body, actor, actorType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit change = %d %s", rec.Code, rec.Body)
	}
	return resp["changeId"].(string)
}

// approveAs 以某角色审批一次。
func approveAs(t *testing.T, h http.Handler, changeID, role, actor string) {
	t.Helper()
	rec, _ := doAs(t, h, "POST", "/api/v1/changes/"+changeID+"/approve",
		`{"role":"`+role+`","comment":"ok"}`, actor, "engineer")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve %s as %s = %d %s", changeID, actor, rec.Code, rec.Body)
	}
}
