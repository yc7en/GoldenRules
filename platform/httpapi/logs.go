package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"goldenrules/platform/gov"
	"goldenrules/platform/obs"
)

// POST /api/v1/logs —— 决策日志入库（引擎把结构化日志推过来，平台负责存与查）
//
// 入参可单条（`log`）也可批量（`items`）；原始快照可选，不带就只留脱敏视图。
func (s *Server) ingestLogs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Log         *obs.LogEntry    `json:"log"`
		RawSnapshot json.RawMessage  `json:"rawSnapshot"`
		Items       []obs.IngestItem `json:"items"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	items := req.Items
	if req.Log != nil {
		items = append(items, obs.IngestItem{Log: *req.Log, RawSnapshot: req.RawSnapshot})
	}
	res, err := s.deps.Obs.Ingest(items)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// GET /api/v1/logs —— 决策日志查询（脱敏视图）
func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 50)
	if limit > 500 {
		limit = 500
	}
	query := obs.Query{
		TenantID:       q.Get("tenantId"),
		Scene:          q.Get("scene"),
		Decision:       q.Get("decision"),
		ReasonCode:     q.Get("reasonCode"),
		PackageVersion: q.Get("packageVersion"),
		From:           q.Get("from"),
		To:             q.Get("to"),
		Limit:          limit,
		Offset:         atoiDefault(q.Get("offset"), 0),
	}
	if v := q.Get("degraded"); v != "" {
		b := v == "true" || v == "1"
		query.Degraded = &b
	}
	items, total := s.deps.Obs.Query(query)
	if items == nil {
		items = []obs.StoredLog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"limit":   query.Limit,
		"offset":  query.Offset,
		"items":   items,
		"storage": storageInfo(s),
	})
}

// GET /api/v1/logs/stats —— 观测面板汇总
func (s *Server) logStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Obs.Stats(r.URL.Query().Get("tenantId")))
}

// GET /api/v1/logs/{id} —— 单条日志（脱敏视图 + 内容摘要）
func (s *Server) getLog(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.deps.Obs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, r, http.StatusNotFound, obs.CodeLogNotFound, "日志不存在: "+r.PathValue("id"), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logId":       rec.LogID,
		"entry":       rec.Entry,
		"contentHash": rec.ContentHash,
		"receivedAt":  rec.ReceivedAt,
		"raw":         rec.Raw,
	})
}

// POST /api/v1/logs/{id}/replay —— 纯快照回放（`contracts/03` §8）
//
// 安全要点：回放要读原始快照，所以它是**一次受审计的越权行为**——
// 无论成功失败都记 `REPLAY_EXECUTED` / `LOG_RAW_ACCESSED`，谁解的密必须有据可查。
func (s *Server) replayLog(w http.ResponseWriter, r *http.Request) {
	logID := r.PathValue("id")
	res, err := s.deps.Obs.Replay(logID, s.deps.Replayer)
	if s.deps.Gov != nil {
		ev := gov.AuditEvent{
			Action:    gov.ActionReplayExecuted,
			Actor:     actor(r),
			ActorType: actorType(r),
			Detail:    "回放日志 " + logID,
		}
		if err != nil {
			ev.Detail += "（失败：" + err.Error() + "）"
		} else {
			ev.Detail += "；结论一致=" + boolText(res.Match)
			if len(res.Diff) > 0 {
				ev.Detail += "；差异：" + joinDiff(res.Diff)
			}
			ev.Action = gov.ActionReplayExecuted
		}
		s.deps.Gov.Record(ev)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "backend": s.deps.Replayer.Describe()})
}

// storageInfo 把「日志存到哪、能不能回放」直接放进列表响应：
// 前端不必猜为什么回放按钮是灰的。
func storageInfo(s *Server) map[string]any {
	replay := false
	backend := ""
	if s.deps.Replayer != nil {
		replay = true
		backend = s.deps.Replayer.Describe()
	}
	return map[string]any{
		"rawEnabled":      s.deps.Obs.RawEnabled(),
		"retentionDays":   s.deps.Obs.RetentionDays(),
		"replayAvailable": replay,
		"replayBackend":   backend,
		"maskedViewOnly":  !s.deps.Obs.RawEnabled(),
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func joinDiff(diff []string) string {
	out := ""
	for i, d := range diff {
		if i > 0 {
			out += "；"
		}
		out += d
	}
	return out
}
