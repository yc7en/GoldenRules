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

// Replayer 回放执行器：把「历史原始快照 + 指定版本」重新跑一遍决策。
//
// 为什么是接口而不是直接写 HTTP：回放是**离线动作**（`contracts/03` §8.1），
// 不该让平台在编译期依赖决策服务的地址；测试里注入假实现也比起进程便宜。
type Replayer interface {
	// Decide 用给定快照与上下文求值，返回引擎的 DecisionResult 原文。
	Decide(snapshot, execCtx json.RawMessage) (json.RawMessage, error)
	// Describe 说明回放后端（写进响应，便于排查「回放结果为什么和线上不一样」）。
	Describe() string
}

// HTTPReplayer 通过决策服务的 `/v1/decide` 回放。
type HTTPReplayer struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPReplayer 构造 HTTP 回放器；baseURL 为空时返回 nil（= 回放不可用）。
func NewHTTPReplayer(baseURL string) *HTTPReplayer {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &HTTPReplayer{BaseURL: baseURL, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (r *HTTPReplayer) Describe() string { return "gr-server " + r.BaseURL }

// Decide 调 `/v1/decide`。注意用 decide 而不是 trial：回放要的是与线上同一入口的语义。
func (r *HTTPReplayer) Decide(snapshot, execCtx json.RawMessage) (json.RawMessage, error) {
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

// Replay 纯快照重算并比对（`contracts/03` §8）。
//
// 比对口径只取「可判定的三样」：`decision`、`reasonCode`、`outputs`。
// 命中路径（`hitRules`）原样带回但不参与判定——它含规则 ID 与版本，属于解释性字段，
// 用它判定会把「规则顺序调整但结论相同」误判成回放失败。
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

	// 上下文按日志重建：版本钉死在日志记录的那一版，否则回放会被当前生效版本污染。
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

// diffOutputs 只报「值不同 / 只在一边」的变量，避免把 key 顺序差异当成不一致。
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
