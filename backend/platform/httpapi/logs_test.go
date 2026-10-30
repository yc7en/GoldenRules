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

// 密钥轮换：自述 → 轮换 → 老日志仍可回放 → 重加密 → 审计留痕。
//
// 这条路径的价值全在「轮换后历史数据还能读」：所以断言里必须真的回放一次老日志。
func TestLogKeyRotationOverHTTP(t *testing.T) {
	h := newObsTestServer(t, true, true)

	// 1 自述：当前钥、来源、保留集
	rec, resp := do(t, h, "GET", "/api/v1/logs/key", "")
	if rec.Code != http.StatusOK || resp["configured"] != true {
		t.Fatalf("密钥自述 = %d %s", rec.Code, rec.Body)
	}
	first := resp["key"].(map[string]any)
	oldKeyID := first["keyId"].(string)
	if oldKeyID == "" || first["algo"] != "AES-256-GCM" {
		t.Fatalf("自述字段不全: %s", rec.Body)
	}
	if _, leaked := first["key"]; leaked {
		t.Fatal("自述不得返回密钥本身")
	}

	// 2 入库一条带原始快照的日志
	body := `{"log":` + engineLog + `,"rawSnapshot":{"snapshotId":"snap-001"}}`
	rec, resp = do(t, h, "POST", "/api/v1/logs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest = %d %s", rec.Code, rec.Body)
	}
	logID := resp["logIds"].([]any)[0].(string)

	// 3 轮换到新钥
	newKey := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rotate", `{"key":"`+newKey+`","reason":"季度轮换"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body)
	}
	key2 := resp["key"].(map[string]any)
	if key2["keyId"] == oldKeyID {
		t.Fatalf("轮换后 keyId 未变: %v", key2["keyId"])
	}
	retained := key2["retainedKeyIds"].([]any)
	if len(retained) != 1 || retained[0] != oldKeyID {
		t.Fatalf("旧钥应在保留集: %v", retained)
	}

	// 4 关键断言：轮换前写入的日志仍可回放
	rec, resp = do(t, h, "POST", "/api/v1/logs/"+logID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("轮换后旧日志应可回放，实际 %d %s", rec.Code, rec.Body)
	}

	// 5 重加密：把历史密文搬到当前钥
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rewrap", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrap = %d %s", rec.Code, rec.Body)
	}
	if resp["rewrapped"] != float64(1) {
		t.Fatalf("应重加密 1 条，实际 %v", resp["rewrapped"])
	}
	// 幂等
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rewrap", "")
	if rec.Code != http.StatusOK || resp["rewrapped"] != float64(0) {
		t.Fatalf("重复重加密应为 0，实际 %d %s", rec.Code, rec.Body)
	}

	// 6 审计：轮换与重加密各留一条
	rec, resp = do(t, h, "GET", "/api/v1/audit?limit=50", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body)
	}
	raw, _ := json.Marshal(resp)
	for _, action := range []string{"LOG_KEY_ROTATED", "LOG_KEY_REWRAPPED"} {
		if !strings.Contains(string(raw), action) {
			t.Fatalf("审计缺少 %s: %s", action, raw)
		}
	}
}

// 轮换入参校验：既没给 key 也没说从托管源取 → 400，而不是「静默不轮换」。
func TestLogKeyRotateValidation(t *testing.T) {
	h := newObsTestServer(t, true, true)
	rec, resp := do(t, h, "POST", "/api/v1/logs/key/rotate", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空入参应 400，实际 %d %s", rec.Code, rec.Body)
	}
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rotate", `{"key":"short"}`)
	if rec.Code != http.StatusBadRequest || resp["errorCode"] != "OBS_KEY_INVALID" {
		t.Fatalf("非法密钥应 400 OBS_KEY_INVALID，实际 %d %s", rec.Code, rec.Body)
	}
}

// 无密钥部署：自述要说清「只有脱敏视图」，轮换要报 422 而不是 500。
func TestLogKeyEndpointsWithoutKey(t *testing.T) {
	h := newObsTestServer(t, false, true)
	rec, resp := do(t, h, "GET", "/api/v1/logs/key", "")
	if rec.Code != http.StatusOK || resp["configured"] != false {
		t.Fatalf("无密钥自述 = %d %s", rec.Code, rec.Body)
	}
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rotate", `{"key":"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"}`)
	if rec.Code != http.StatusUnprocessableEntity || resp["errorCode"] != "OBS_KEY_MISSING" {
		t.Fatalf("无密钥轮换应 422，实际 %d %s", rec.Code, rec.Body)
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

// 类型化 nil 的回放后端不能把日志列表打成 500。
//
// 这个坑真实踩过：`NewHTTPReplayer("")` 返回 `*HTTPReplayer(nil)`，装进接口后
// `deps.Replayer != nil` 为真，handler 一调 Describe 就空指针 panic。
func TestNilTypedReplayerDoesNotPanic(t *testing.T) {
	// 用类型化 nil 装配：模拟「没配 --engine-url」的真实结果
	h := newObsEnv(t, true, (*obs.HTTPReplayer)(nil))
	rec, body := do(t, h, "GET", "/api/v1/logs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列日志 = %d %s", rec.Code, rec.Body)
	}
	storage := body["storage"].(map[string]any)
	if storage["replayAvailable"] != false {
		t.Fatalf("未配置回放后端应报 replayAvailable=false: %v", storage)
	}
}
