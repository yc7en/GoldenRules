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

// GET /api/v1/logs/key —— 密钥自述（当前 keyId / 来源 / 保留钥）
//
// 为什么不返回密钥本身：平台只需要说清「现在用哪把钥匙、还能解哪些老密文」，
// 密钥值一旦走 HTTP 就会出现在网关日志、浏览器缓存、代理里。
func (s *Server) getLogKey(w http.ResponseWriter, r *http.Request) {
	info := s.deps.Obs.KeyInfo()
	if info == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"rawEnabled": false,
			"note":       "未配置 -log-key / GOLDENRULES_LOG_KEY：只存脱敏视图，原始快照不可用",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"key":        info,
	})
}

// POST /api/v1/logs/key/rotate —— 轮换原始快照加密密钥（`contracts/03` §8.2）
//
// 语义刻意收窄：**只改变此后新写入用哪把钥匙**，历史密文继续用旧钥解密。
// 想真正让旧钥下线，必须先 `POST /api/v1/logs/key/rewrap` 把历史密文重加密，
// 否则删钥 = 历史原始快照永久不可读。
//
// 入参：`{"key": "<32字节 hex/base64>"}`；`{"fromProvider": true}` 表示向 KMS 要当前钥。
func (s *Server) rotateLogKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key          string `json:"key"`
		FromProvider bool   `json:"fromProvider"`
		Reason       string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	if !req.FromProvider && req.Key == "" {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "必须提供 key，或置 fromProvider=true 由托管源提供", nil)
		return
	}
	const maxKeyBytes = 512
	if len(req.Key) > maxKeyBytes {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "key 过长（不应超过 512 字节）", nil)
		return
	}

	keyID, rerr := s.rotateKeySecurely(r, req.Key, req.FromProvider)
	if rerr != nil {
		fail(w, r, rerr)
		return
	}
	info := s.deps.Obs.KeyInfo()
	writeJSON(w, http.StatusOK, map[string]any{
		"keyId":  keyID,
		"key":    info,
		"note":   "轮换只影响新写入的密文；历史密文仍用旧钥解密。旧钥未删除。",
		"reason": req.Reason,
	})
}

// rotateKeySecurely 轮换并留痕（轮换改变了「谁能读历史原文」的前提）。
func (s *Server) rotateKeySecurely(r *http.Request, key string, fromProvider bool) (string, *obs.Error) {
	var (
		keyID string
		err   *obs.Error
	)
	if fromProvider {
		keyID, err = s.deps.Obs.RotateKeyFromProvider()
	} else {
		keyID, err = s.deps.Obs.RotateKey(key)
	}
	if s.deps.Gov != nil {
		ev := gov.AuditEvent{
			Action:    gov.ActionLogKeyRotated,
			Actor:     actor(r),
			ActorType: actorType(r),
			Detail:    "轮换原始快照密钥 → " + keyID,
		}
		if fromProvider {
			ev.Detail += "（来源：托管密钥服务）"
		}
		if err != nil {
			ev.Detail += "（失败：" + err.Message + "）"
		}
		s.deps.Gov.Record(ev)
	}
	return keyID, err
}

// POST /api/v1/logs/key/rewrap —— 把历史密文重加密到当前钥。
//
// 这是「下线旧钥」的前置动作：不重加密就删旧钥，等于让历史原始快照永久不可读。
// 返回处理条数；半途失败时已成功的条目已落库，可安全重跑（幂等：同钥跳过）。
func (s *Server) rewrapLogKey(w http.ResponseWriter, r *http.Request) {
	done, err := s.deps.Obs.RewrapRaw()
	if s.deps.Gov != nil {
		ev := gov.AuditEvent{
			Action:    gov.ActionLogKeyRewrapped,
			Actor:     actor(r),
			ActorType: actorType(r),
			Detail:    "重加密历史原始快照 " + strconv.Itoa(done) + " 条",
		}
		if err != nil {
			ev.Detail += "（失败：" + err.Message + "）"
		}
		s.deps.Gov.Record(ev)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	info := s.deps.Obs.KeyInfo()
	writeJSON(w, http.StatusOK, map[string]any{
		"rewrapped": done,
		"key":       info,
		"note":      "重加密完成，历史密文已全部可被当前钥解开",
	})
}

// storageInfo 把「日志存到哪、能不能回放」直接放进列表响应：
// 前端不必猜为什么回放按钮是灰的。
func storageInfo(s *Server) map[string]any {
	replay := false
	backend := ""
	// 注意不要只判 `!= nil`：没配 --engine-url 时装配出来的是**类型化 nil**
	// （接口不为 nil、指针为 nil）。用「能不能自述」当判据，才能如实反映可用性。
	if s.deps.Replayer != nil {
		if desc := s.deps.Replayer.Describe(); desc != "" {
			replay = true
			backend = desc
		}
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
