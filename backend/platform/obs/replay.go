package obs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Replayer is the replay executor: it re-runs a decision from "historical raw snapshot + a specified version".
//
// Why an interface instead of writing HTTP directly: replay is an **offline action** (`contracts/03` §8.1), and
// the platform should not depend on the decision service's address at compile time; injecting a fake in tests is also cheaper than spawning a process.
type Replayer interface {
	// Decide evaluates with the given snapshot and context and returns the engine's DecisionResult verbatim.
	Decide(snapshot, execCtx json.RawMessage) (json.RawMessage, error)
	// Describe describes the replay backend (written into the response, to help explain "why the replay differs from production").
	Describe() string
}

// HTTPReplayer replays through the decision service's `/v1/decide`.
type HTTPReplayer struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPReplayer builds the HTTP replayer; an empty baseURL returns nil (= replay unavailable).
func NewHTTPReplayer(baseURL string) *HTTPReplayer {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &HTTPReplayer{BaseURL: baseURL, Client: &http.Client{Timeout: 5 * time.Second}}
}

// Describe is the replay backend's self-description.
//
// A nil receiver returns an empty string instead of panicking: `NewHTTPReplayer("")` returns a **typed nil**
// (a nil `*HTTPReplayer`) which is not nil once stored in a `Replayer` interface. Such a "looks non-nil
// but is actually nil" value leaking into a handler would turn the whole log-list endpoint into a 500 — hence this guard.
func (r *HTTPReplayer) Describe() string {
	if r == nil {
		return ""
	}
	return "gr-server " + r.BaseURL
}

// Decide calls `/v1/decide`. Note it uses decide rather than trial: replay needs the semantics of the same entry point as production.
func (r *HTTPReplayer) Decide(snapshot, execCtx json.RawMessage) (json.RawMessage, error) {
	if r == nil {
		return nil, fmt.Errorf("回放后端未配置（--engine-url 为空）")
	}
	body, err := json.Marshal(map[string]json.RawMessage{
		"context":  execCtx,
		"snapshot": snapshot,
	})
	if err != nil {
		return nil, fmt.Errorf("组装回放请求失败: %w", err)
	}
	resp, err := r.Client.Post(r.BaseURL+"/v1/decide", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("回放请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读回放响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("回放返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.RawMessage(raw), nil
}

// Replay recomputes from the raw snapshot only and compares (`contracts/03` §8).
//
// The comparison only takes "the three decidable things": `decision`, `reasonCode`, `outputs`.
// The hit path (`hitRules`) is returned as-is but does not participate in the verdict — it contains rule IDs and versions and is explanatory,
// so judging by it would misclassify "rule order changed but the conclusion is the same" as a replay failure.
func (s *Service) Replay(logID string, r Replayer) (ReplayResult, error) {
	if r == nil {
		return ReplayResult{}, errf(CodeReplayUnavailable,
			"未配置决策服务地址（-engine-url），回放不可用")
	}
	rec, ok := s.Get(logID)
	if !ok {
		return ReplayResult{}, errf(CodeLogNotFound, "日志不存在: "+logID)
	}
	snapshot, rerr := s.RawSnapshot(logID)
	if rerr != nil {
		return ReplayResult{}, rerr
	}

	// Rebuild the context from the log: the version is pinned to the one recorded in the log, otherwise the replay would be polluted by the currently active version.
	ctx := map[string]any{
		"tenantId":    rec.Entry.TenantID,
		"scene":       rec.Entry.Scene,
		"requestTime": rec.Entry.ExecutedAt,
		"traceId":     rec.Entry.TraceID,
	}
	if rec.Entry.PackageVersion != "" {
		ctx["packageVersion"] = rec.Entry.PackageVersion
	}
	if rec.Entry.RulesetID != "" {
		ctx["rulesetId"] = rec.Entry.RulesetID
	}
	if rec.Entry.RulesetVersion != "" {
		ctx["rulesetVersion"] = rec.Entry.RulesetVersion
	}
	execCtx, err := json.Marshal(ctx)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("组装回放上下文失败: %w", err)
	}

	raw, err := r.Decide(snapshot, execCtx)
	if err != nil {
		return ReplayResult{}, errf(CodeReplayFailed, err.Error())
	}

	var got struct {
		Decision   string                     `json:"decision"`
		ReasonCode string                     `json:"reasonCode"`
		Outputs    map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return ReplayResult{}, errf(CodeReplayFailed, "回放响应不是 DecisionResult: "+err.Error())
	}

	res := ReplayResult{LogID: logID, Replay: raw, At: nowString(s.now())}
	if got.Decision != rec.Entry.Decision {
		res.Diff = append(res.Diff, fmt.Sprintf("decision: %s → %s", rec.Entry.Decision, got.Decision))
	}
	if got.ReasonCode != rec.Entry.ReasonCode {
		res.Diff = append(res.Diff, fmt.Sprintf("reasonCode: %s → %s", rec.Entry.ReasonCode, got.ReasonCode))
	}
	if d := diffOutputs(rec.Entry.Outputs, got.Outputs); d != "" {
		res.Diff = append(res.Diff, "outputs: "+d)
	}
	res.Match = len(res.Diff) == 0
	if !res.Match {
		res.Note = "回放结论与日志不一致：先确认规则包版本是否仍可加载，再看原始快照是否完整"
	}
	return res, nil
}

// diffOutputs only reports variables that "differ in value / exist on one side only", so key-order differences are not taken for mismatches.
func diffOutputs(want, got map[string]json.RawMessage) string {
	var parts []string
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			parts = append(parts, k+" 缺失")
			continue
		}
		if !jsonEqual(wv, gv) {
			parts = append(parts, k+": "+string(wv)+" → "+string(gv))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			parts = append(parts, k+" 多出")
		}
	}
	return strings.Join(parts, "；")
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return string(a) == string(b)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return string(a) == string(b)
	}
	return fmt.Sprint(av) == fmt.Sprint(bv)
}
