package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"goldenrules/platform/obs"
)

// 一条引擎形态的决策日志（字段名与 Rust `DecisionLogEntry` 一致，camelCase）。
const engineLog = `{
  "snapshotId": "snap-001",
  "traceId": "trace-001",
  "tenantId": "t-demo",
  "scene": "withdraw-risk-check",
  "factSchemaVersion": "catalog-v2026.10.8",
  "packageId": "pkg-withdraw",
  "packageVersion": "v1",
  "rulesetId": "rs-amount-limit",
  "rulesetVersion": "rs-1",
  "maskedFacts": {"request.amount": "1500", "customer.idCardNo": "1101****1234"},
  "maskedFields": ["customer.idCardNo"],
  "hitRules": [{"ruleId":"R1","version":"v4"}],
  "outputs": {"riskLevel": "MEDIUM"},
  "decision": "REVIEW",
  "reasonCode": "AMOUNT_REVIEW",
  "degraded": false,
  "degradedReasons": [],
  "engineVersion": "0.1.0",
  "executionTimeMs": 0.42,
  "evaluatedRules": 3,
  "executedAt": "2026-10-30T11:40:00Z"
}`

// 决策日志：入库 → 查询 → 详情 → 回放（含审计留痕）。
func TestDecisionLogsOverHTTP(t *testing.T) {
	h := newObsTestServer(t, true, true)

	// 1 入库（带原始快照）
	body := `{"log":` + engineLog + `,"rawSnapshot":{"snapshotId":"snap-001","facts":{"request.amount":"1500"}}}`
	rec, resp := do(t, h, "POST", "/api/v1/logs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest = %d %s", rec.Code, rec.Body)
	}
	if resp["accepted"] != float64(1) || resp["rawStored"] != float64(1) {
		t.Fatalf("入库结果异常: %s", rec.Body)
	}
	logIDs := resp["logIds"].([]any)
	logID := logIDs[0].(string)

	// 2 幂等
	rec, resp = do(t, h, "POST", "/api/v1/logs", body)
	if rec.Code != http.StatusCreated || resp["duplicate"] != float64(1) {
		t.Fatalf("重复入库应幂等: %d %s", rec.Code, rec.Body)
	}

	// 3 查询：脱敏视图 + 存储能力说明
	rec, resp = do(t, h, "GET", "/api/v1/logs?scene=withdraw-risk-check&decision=REVIEW", "")
	if rec.Code != http.StatusOK || resp["total"] != float64(1) {
		t.Fatalf("查询 = %d %s", rec.Code, rec.Body)
	}
	storage := resp["storage"].(map[string]any)
	if storage["rawEnabled"] != true || storage["replayAvailable"] != true || storage["retentionDays"] != float64(90) {
		t.Fatalf("存储说明异常: %v", storage)
	}
	item := resp["items"].([]any)[0].(map[string]any)
	entry := item["entry"].(map[string]any)
	if entry["decision"] != "REVIEW" || entry["maskedFields"] == nil {
		t.Fatalf("日志内容异常: %v", entry)
	}
	if !strings.HasPrefix(item["contentHash"].(string), "sha256:") {
		t.Fatalf("缺内容摘要: %v", item["contentHash"])
	}
	// 脱敏视图里不应出现明文身份证号
	if strings.Contains(rec.Body.String(), "110101199001011234") {
		t.Fatalf("响应泄露了未脱敏字段")
	}

	// 4 详情
	rec, resp = do(t, h, "GET", "/api/v1/logs/"+logID, "")
	if rec.Code != http.StatusOK || resp["logId"] != logID {
		t.Fatalf("详情 = %d %s", rec.Code, rec.Body)
	}
	raw := resp["raw"].(map[string]any)
	if raw["algo"] != "AES-256-GCM" || raw["ciphertext"] != nil {
		t.Fatalf("详情不应返回密文: %v", raw)
	}

	// 5 统计
	rec, resp = do(t, h, "GET", "/api/v1/logs/stats?tenantId=t-demo", "")
	if rec.Code != http.StatusOK || resp["total"] != float64(1) {
		t.Fatalf("stats = %d %s", rec.Code, rec.Body)
	}

	// 6 回放
	rec, resp = do(t, h, "POST", "/api/v1/logs/"+logID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body)
	}
	result := resp["result"].(map[string]any)
	if result["match"] != true {
		t.Fatalf("回放应一致: %s", rec.Body)
	}
	if resp["backend"] != "stub-engine" {
		t.Fatalf("回放后端说明缺失: %v", resp["backend"])
	}

	// 7 回放动作留痕
	rec, resp = do(t, h, "GET", "/api/v1/audit", "")
	found := false
	for _, it := range resp["items"].([]any) {
		if it.(map[string]any)["action"] == "REPLAY_EXECUTED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("回放应写审计: %s", rec.Body)
	}
}

// 没配密钥：只存脱敏视图，回放 422 且原因明确。
func TestLogsWithoutCryptoDegrade(t *testing.T) {
	h := newObsTestServer(t, false, true)
	body := `{"log":` + engineLog + `,"rawSnapshot":{"a":1}}`
	rec, resp := do(t, h, "POST", "/api/v1/logs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest = %d %s", rec.Code, rec.Body)
	}
	if resp["rawStored"] != float64(0) || resp["rawSkipped"] != float64(1) {
		t.Fatalf("未配密钥应跳过原文: %s", rec.Body)
	}
	if warns, _ := resp["warnings"].([]any); len(warns) == 0 {
		t.Fatalf("应提示回放不可用: %s", rec.Body)
	}
	logID := resp["logIds"].([]any)[0].(string)

	// 没密钥时连"有没有原文"都判不了，直接报 KEY_MISSING（比 RAW_UNAVAILABLE 更准确）
	rec, resp = do(t, h, "POST", "/api/v1/logs/"+logID+"/replay", "")
	if rec.Code != http.StatusUnprocessableEntity || resp["errorCode"] != "OBS_KEY_MISSING" {
		t.Fatalf("无密钥回放应 422 OBS_KEY_MISSING，实际 %d %s", rec.Code, rec.Body)
	}
}

// 必填字段缺失 → 422；未知日志 → 404。
func TestLogsValidationAndNotFound(t *testing.T) {
	h := newObsTestServer(t, true, true)

	bad := strings.Replace(engineLog, `"traceId": "trace-001",`, ``, 1)
	rec, resp := do(t, h, "POST", "/api/v1/logs", `{"log":`+bad+`}`)
	if rec.Code != http.StatusUnprocessableEntity || resp["errorCode"] != "OBS_LOG_INVALID" {
		t.Fatalf("缺 traceId 应 422，实际 %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(resp["message"].(string), "traceId") {
		t.Fatalf("诊断应指出缺失字段: %v", resp["message"])
	}

	rec, resp = do(t, h, "GET", "/api/v1/logs/LOG-NOPE", "")
	if rec.Code != http.StatusNotFound || resp["errorCode"] != "OBS_LOG_NOT_FOUND" {
		t.Fatalf("未知日志应 404，实际 %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------

// stubReplayer 模拟决策服务：回放结论取日志里的 decision（= 恒一致），
// 只用于验证平台侧编排与比对，不验证引擎。
type stubReplayer struct{}

func (stubReplayer) Describe() string { return "stub-engine" }

func (stubReplayer) Decide(snapshot, execCtx json.RawMessage) (json.RawMessage, error) {
	var ctx struct {
		Scene string `json:"scene"`
	}
	_ = json.Unmarshal(execCtx, &ctx)
	return json.Marshal(map[string]any{
		"decision":   "REVIEW",
		"reasonCode": "AMOUNT_REVIEW",
		"outputs":    map[string]string{"riskLevel": "MEDIUM"},
	})
}

var _ obs.Replayer = stubReplayer{}
