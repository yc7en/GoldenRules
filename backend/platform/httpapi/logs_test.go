package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"goldenrules/platform/obs"
)

// A decision log in engine shape (field names match the Rust `DecisionLogEntry`, camelCase).
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

// Decision log: ingest → query → detail → replay (including the audit trail).
func TestDecisionLogsOverHTTP(t *testing.T) {
	h := newObsTestServer(t, true, true)

	// 1 ingest (with a raw snapshot)
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

	// 2 idempotency
	rec, resp = do(t, h, "POST", "/api/v1/logs", body)
	if rec.Code != http.StatusCreated || resp["duplicate"] != float64(1) {
		t.Fatalf("重复入库应幂等: %d %s", rec.Code, rec.Body)
	}

	// 3 query: masked view + storage capability description
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
	// the masked view must not contain a plaintext ID card number
	if strings.Contains(rec.Body.String(), "110101199001011234") {
		t.Fatalf("响应泄露了未脱敏字段")
	}

	// 4 detail
	rec, resp = do(t, h, "GET", "/api/v1/logs/"+logID, "")
	if rec.Code != http.StatusOK || resp["logId"] != logID {
		t.Fatalf("详情 = %d %s", rec.Code, rec.Body)
	}
	raw := resp["raw"].(map[string]any)
	if raw["algo"] != "AES-256-GCM" || raw["ciphertext"] != nil {
		t.Fatalf("详情不应返回密文: %v", raw)
	}

	// 5 stats
	rec, resp = do(t, h, "GET", "/api/v1/logs/stats?tenantId=t-demo", "")
	if rec.Code != http.StatusOK || resp["total"] != float64(1) {
		t.Fatalf("stats = %d %s", rec.Code, rec.Body)
	}

	// 6 replay
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

	// 7 the replay action is audited
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

// No key configured: only the masked view is stored, and replay returns 422 with an explicit reason.
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

	// Without a key even "is there a raw snapshot" cannot be decided, so report KEY_MISSING directly (more accurate than RAW_UNAVAILABLE)
	rec, resp = do(t, h, "POST", "/api/v1/logs/"+logID+"/replay", "")
	if rec.Code != http.StatusUnprocessableEntity || resp["errorCode"] != "OBS_KEY_MISSING" {
		t.Fatalf("无密钥回放应 422 OBS_KEY_MISSING，实际 %d %s", rec.Code, rec.Body)
	}
}

// A missing required field → 422; an unknown log → 404.
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

// Key rotation: self-description → rotate → old logs still replayable → rewrap → audit trail.
//
// The whole value of this path is "historic data stays readable after rotation": so the assertions must actually replay an old log once.
func TestLogKeyRotationOverHTTP(t *testing.T) {
	h := newObsTestServer(t, true, true)

	// 1 self-description: current key, source, retained set
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

	// 2 ingest one log with a raw snapshot
	body := `{"log":` + engineLog + `,"rawSnapshot":{"snapshotId":"snap-001"}}`
	rec, resp = do(t, h, "POST", "/api/v1/logs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest = %d %s", rec.Code, rec.Body)
	}
	logID := resp["logIds"].([]any)[0].(string)

	// 3 rotate to a new key
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

	// 4 key assertion: a log written before rotation is still replayable
	rec, resp = do(t, h, "POST", "/api/v1/logs/"+logID+"/replay", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("轮换后旧日志应可回放，实际 %d %s", rec.Code, rec.Body)
	}

	// 5 rewrap: move the historic ciphertext to the current key
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rewrap", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrap = %d %s", rec.Code, rec.Body)
	}
	if resp["rewrapped"] != float64(1) {
		t.Fatalf("应重加密 1 条，实际 %v", resp["rewrapped"])
	}
	// idempotency
	rec, resp = do(t, h, "POST", "/api/v1/logs/key/rewrap", "")
	if rec.Code != http.StatusOK || resp["rewrapped"] != float64(0) {
		t.Fatalf("重复重加密应为 0，实际 %d %s", rec.Code, rec.Body)
	}

	// 6 audit: one entry each for rotation and rewrap
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

// Rotate input validation: neither a key nor a request to take one from the managed source → 400, not a silent no-op rotation.
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

// Keyless deployment: self-description must state "masked view only", and rotation must return 422 rather than 500.
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

// stubReplayer mimics the decision service: the replay conclusion is taken from the log's decision (= always consistent),
// and it only serves to verify the platform-side orchestration and comparison, not the engine.
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

// A typed-nil replay backend must not turn the log list into a 500.
//
// This pitfall was hit for real: `NewHTTPReplayer("")` returns `*HTTPReplayer(nil)`; once that is stored in the interface
// `deps.Replayer != nil` holds, so the handler panics on a nil pointer the moment it calls Describe.
func TestNilTypedReplayerDoesNotPanic(t *testing.T) {
	// Assemble with a typed nil: reproduce the real outcome of "--engine-url not configured"
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
