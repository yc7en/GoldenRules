package obs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"goldenrules/platform/persist"
)

const testKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func sampleLog(snapshot, trace, decision string) LogEntry {
	return LogEntry{
		SnapshotID:        snapshot,
		TraceID:           trace,
		TenantID:          "t-demo",
		Scene:             "withdraw-risk-check",
		FactSchemaVersion: "catalog-v2026.10.8",
		PackageID:         "pkg-withdraw",
		PackageVersion:    "v1",
		RulesetID:         "rs-amount-limit",
		RulesetVersion:    "rs-1",
		MaskedFacts:       map[string]json.RawMessage{"request.amount": json.RawMessage(`"1500"`)},
		MaskedFields:      []string{"customer.idCardNo"},
		HitRules:          []json.RawMessage{json.RawMessage(`{"ruleId":"R1","version":"v4"}`)},
		Outputs:           map[string]json.RawMessage{"riskLevel": json.RawMessage(`"MEDIUM"`)},
		Decision:          decision,
		ReasonCode:        "AMOUNT_REVIEW",
		EngineVersion:     "0.1.0",
		ExecutionTimeMs:   0.42,
		EvaluatedRules:    3,
		ExecutedAt:        "2026-10-30T11:40:00Z",
	}
}

func newSvc(t *testing.T, withKey bool) (*Service, *persist.Store[RawDoc]) {
	t.Helper()
	dir := t.TempDir()
	logs, err := persist.Open(filepath.Join(dir, "logs.json"), func() LogDoc { return LogDoc{Seq: map[string]int{}} })
	if err != nil {
		t.Fatalf("open logs: %v", err)
	}
	raw, err := persist.Open(filepath.Join(dir, "raw.json"), func() RawDoc { return RawDoc{Blobs: map[string]RawSnapshotRef{}} })
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	var box *Box
	if withKey {
		box, err = NewBox(testKey)
		if err != nil {
			t.Fatalf("box: %v", err)
		}
	}
	svc := New(logs, raw, box).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})
	return svc, raw
}

// Ingest is idempotent: pushing the same snapshotId+traceId again is recorded only once.
func TestIngestIsIdempotent(t *testing.T) {
	svc, _ := newSvc(t, true)
	item := IngestItem{Log: sampleLog("snap-1", "trace-1", "REVIEW")}

	res, err := svc.Ingest([]IngestItem{item})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Accepted != 1 || len(res.LogIDs) != 1 {
		t.Fatalf("首次入库应接受 1 条: %+v", res)
	}
	if res.LogIDs[0] != "LOG-20261030-0001" {
		t.Fatalf("日志号格式异常: %s", res.LogIDs[0])
	}

	res, err = svc.Ingest([]IngestItem{item})
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if res.Accepted != 0 || res.Duplicate != 1 {
		t.Fatalf("重复推送应幂等: %+v", res)
	}
	if _, total := svc.Query(Query{}); total != 1 {
		t.Fatalf("库内应只有 1 条，实际 %d", total)
	}
}

// Missing A-4 required fields are rejected outright.
func TestIngestValidatesRequiredFields(t *testing.T) {
	svc, _ := newSvc(t, true)
	bad := sampleLog("snap-x", "", "REVIEW") // missing traceId
	_, err := svc.Ingest([]IngestItem{{Log: bad}})
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != CodeInvalidLog {
		t.Fatalf("缺字段应被拒，实际 %v", err)
	}
	if _, total := svc.Query(Query{}); total != 0 {
		t.Fatalf("非法日志不应入库")
	}

	bad2 := sampleLog("snap-y", "trace-y", "REVIEW")
	bad2.ExecutedAt = "2026/10/30 12:00"
	if _, err := svc.Ingest([]IngestItem{{Log: bad2}}); err == nil {
		t.Fatalf("非 RFC3339 时间应被拒")
	}
}

// The raw snapshot is stored encrypted, and no ciphertext remains in the logs document.
func TestRawSnapshotEncryptedAndSeparated(t *testing.T) {
	svc, rawStore := newSvc(t, true)
	raw := json.RawMessage(`{"snapshotId":"snap-1","facts":{"customer.idCardNo":"110101199001011234"}}`)
	res, err := svc.Ingest([]IngestItem{{Log: sampleLog("snap-1", "trace-1", "REVIEW"), RawSnapshot: raw}})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.RawStored != 1 {
		t.Fatalf("应存 1 份原始快照: %+v", res)
	}
	logID := res.LogIDs[0]

	// the logs document holds only reference metadata, no ciphertext
	rec, _ := svc.Get(logID)
	if rec.Raw == nil || rec.Raw.Ciphertext != "" {
		t.Fatalf("日志视图不应包含密文: %+v", rec.Raw)
	}
	if rec.Raw.Algo != "AES-256-GCM" || rec.Raw.KeyID == "" {
		t.Fatalf("密文元数据不全: %+v", rec.Raw)
	}
	if rec.Raw.ExpiresAt != "2027-01-28T12:00:00Z" { // 90 days later
		t.Fatalf("保留期计算异常: %s", rec.Raw.ExpiresAt)
	}
	// the raw document holds ciphertext, no plaintext
	var blob RawSnapshotRef
	rawStore.View(func(d *RawDoc) { blob = d.Blobs[logID] })
	if blob.Ciphertext == "" {
		t.Fatalf("raw 文档缺密文")
	}
	if string(blob.Ciphertext) == string(raw) {
		t.Fatalf("密文与明文相同")
	}

	// it decrypts back to the original
	got, rerr := svc.RawSnapshot(logID)
	if rerr != nil {
		t.Fatalf("解密失败: %v", rerr)
	}
	if string(got) != string(raw) {
		t.Fatalf("解密结果与原文不一致: %s", got)
	}
}

// With no key only the redacted view is stored, and the caller is explicitly told that replay is unavailable.
func TestIngestWithoutKeyKeepsMaskedViewOnly(t *testing.T) {
	svc, _ := newSvc(t, false)
	if svc.RawEnabled() {
		t.Fatalf("无密钥时不应启用原始快照")
	}
	res, err := svc.Ingest([]IngestItem{{
		Log:         sampleLog("snap-2", "trace-2", "REJECT"),
		RawSnapshot: json.RawMessage(`{"a":1}`),
	}})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Accepted != 1 || res.RawStored != 0 || res.RawSkipped != 1 {
		t.Fatalf("应只存脱敏视图: %+v", res)
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("应提示回放不可用: %+v", res)
	}
	logID := res.LogIDs[0]
	if _, rerr := svc.RawSnapshot(logID); rerr == nil || rerr.Code != CodeKeyMissing {
		t.Fatalf("无密钥读取原文应 %s，实际 %v", CodeKeyMissing, rerr)
	}
}

// Once retention expires the raw snapshot is purged while the redacted log remains.
func TestRetentionPurgesRawOnly(t *testing.T) {
	svc, rawStore := newSvc(t, true)
	svc.WithRetention(1)
	res, err := svc.Ingest([]IngestItem{{
		Log:         sampleLog("snap-3", "trace-3", "REVIEW"),
		RawSnapshot: json.RawMessage(`{"a":1}`),
	}})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	logID := res.LogIDs[0]

	// push the clock past the retention period
	svc.WithClock(func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) })
	purged, err := svc.PurgeExpired()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 1 {
		t.Fatalf("应清理 1 份到期快照，实际 %d", purged)
	}
	var n int
	rawStore.View(func(d *RawDoc) { n = len(d.Blobs) })
	if n != 0 {
		t.Fatalf("raw 文档应已清空，剩 %d", n)
	}
	if _, total := svc.Query(Query{}); total != 1 {
		t.Fatalf("脱敏日志不应被清理")
	}
	if _, rerr := svc.RawSnapshot(logID); rerr == nil || rerr.Code != CodeRawPurged {
		t.Fatalf("过期快照应报 %s，实际 %v", CodeRawPurged, rerr)
	}
}

// Query conditions and paging.
func TestQueryFiltersAndPaginates(t *testing.T) {
	svc, _ := newSvc(t, true)
	var items []IngestItem
	for i, d := range []string{"REVIEW", "REJECT", "REVIEW"} {
		e := sampleLog("snap-q"+string(rune('a'+i)), "trace-q"+string(rune('a'+i)), d)
		e.ExecutedAt = []string{"2026-10-30T10:00:00Z", "2026-10-30T11:00:00Z", "2026-10-30T12:00:00Z"}[i]
		items = append(items, IngestItem{Log: e})
	}
	if _, err := svc.Ingest(items); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got, total := svc.Query(Query{Decision: "REVIEW"}); total != 2 || len(got) != 2 {
		t.Fatalf("按决策过滤应 2 条，实际 %d", total)
	}
	// time descending
	got, _ := svc.Query(Query{})
	if got[0].Entry.ExecutedAt != "2026-10-30T12:00:00Z" {
		t.Fatalf("应按时间倒序，首条 %s", got[0].Entry.ExecutedAt)
	}
	// time window
	if _, total := svc.Query(Query{From: "2026-10-30T10:30:00Z"}); total != 2 {
		t.Fatalf("时间窗过滤应 2 条，实际 %d", total)
	}
	// paging
	page, total := svc.Query(Query{Limit: 1, Offset: 1})
	if total != 3 || len(page) != 1 {
		t.Fatalf("分页异常 total=%d len=%d", total, len(page))
	}

	st := svc.Stats("t-demo")
	if st["total"] != 3 || st["byDecision"].(map[string]int)["REVIEW"] != 2 {
		t.Fatalf("统计异常: %+v", st)
	}
}

// Content digest: changing any field changes it.
func TestContentHashDetectsTamper(t *testing.T) {
	e := sampleLog("snap-h", "trace-h", "REVIEW")
	h1 := e.Hash()
	e.Decision = "REJECT"
	if e.Hash() == h1 {
		t.Fatalf("内容变化后摘要应改变")
	}
}

// ---------------------------------------------------------------------------

// fakeReplayer replays a fixed conclusion, so the comparison logic can be verified without spawning an engine process.
type fakeReplayer struct {
	decision string
	reason   string
	outputs  map[string]json.RawMessage
	err      error
	seenCtx  json.RawMessage
}

func (f *fakeReplayer) Describe() string { return "fake-replayer" }

func (f *fakeReplayer) Decide(snapshot, execCtx json.RawMessage) (json.RawMessage, error) {
	f.seenCtx = execCtx
	if f.err != nil {
		return nil, f.err
	}
	return json.Marshal(map[string]any{
		"decision":   f.decision,
		"reasonCode": f.reason,
		"outputs":    f.outputs,
	})
}

func TestReplayComparesDecisionAndPinsVersions(t *testing.T) {
	svc, _ := newSvc(t, true)
	res, err := svc.Ingest([]IngestItem{{
		Log:         sampleLog("snap-r", "trace-r", "REVIEW"),
		RawSnapshot: json.RawMessage(`{"snapshotId":"snap-r","facts":{}}`),
	}})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	logID := res.LogIDs[0]

	// identical
	fp := &fakeReplayer{decision: "REVIEW", reason: "AMOUNT_REVIEW", outputs: map[string]json.RawMessage{"riskLevel": json.RawMessage(`"MEDIUM"`)}}
	out, err := svc.Replay(logID, fp)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !out.Match || len(out.Diff) != 0 {
		t.Fatalf("应判定一致: %+v", out)
	}
	// the version must be pinned to the one recorded in the log
	var ctx map[string]any
	_ = json.Unmarshal(fp.seenCtx, &ctx)
	if ctx["packageVersion"] != "v1" || ctx["rulesetVersion"] != "rs-1" {
		t.Fatalf("回放上下文未钉版本: %v", ctx)
	}
	if ctx["requestTime"] != "2026-10-30T11:40:00Z" {
		t.Fatalf("回放应使用日志时间作为时间基准: %v", ctx["requestTime"])
	}

	// decision changed
	fp2 := &fakeReplayer{decision: "REJECT", reason: "AMOUNT_REVIEW", outputs: map[string]json.RawMessage{"riskLevel": json.RawMessage(`"MEDIUM"`)}}
	out, err = svc.Replay(logID, fp2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if out.Match || len(out.Diff) != 1 {
		t.Fatalf("决策变化应判不一致: %+v", out)
	}

	// outputs changed
	fp3 := &fakeReplayer{decision: "REVIEW", reason: "AMOUNT_REVIEW", outputs: map[string]json.RawMessage{"riskLevel": json.RawMessage(`"HIGH"`)}}
	out, _ = svc.Replay(logID, fp3)
	if out.Match || len(out.Diff) != 1 {
		t.Fatalf("输出变化应判不一致: %+v", out)
	}
}

func TestReplayUnavailableAndRawMissing(t *testing.T) {
	svc, _ := newSvc(t, true)
	res, _ := svc.Ingest([]IngestItem{{Log: sampleLog("snap-n", "trace-n", "REVIEW")}})
	logID := res.LogIDs[0]

	// no replay backend configured
	_, err := svc.Replay(logID, nil)
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != CodeReplayUnavailable {
		t.Fatalf("未配回放应 %s，实际 %v", CodeReplayUnavailable, err)
	}
	// no raw snapshot stored
	_, err = svc.Replay(logID, &fakeReplayer{decision: "REVIEW", reason: "AMOUNT_REVIEW"})
	if !errors.As(err, &oe) || oe.Code != CodeRawUnavailable {
		t.Fatalf("无原始快照应 %s，实际 %v", CodeRawUnavailable, err)
	}
	// log does not exist
	_, err = svc.Replay("LOG-NOPE", &fakeReplayer{})
	if !errors.As(err, &oe) || oe.Code != CodeLogNotFound {
		t.Fatalf("日志不存在应 %s，实际 %v", CodeLogNotFound, err)
	}
}
