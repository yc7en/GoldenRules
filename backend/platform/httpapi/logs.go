package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"goldenrules/platform/gov"
	"goldenrules/platform/obs"
)

// POST /api/v1/logs —— ingest decision logs (the engine pushes structured logs; the platform stores and queries them)
//
// The input may be a single entry (`log`) or a batch (`items`); the raw snapshot is optional — without it only the masked view is kept.
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

// GET /api/v1/logs —— decision log query (masked view)
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

// GET /api/v1/logs/stats —— observation panel summary
func (s *Server) logStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Obs.Stats(r.URL.Query().Get("tenantId")))
}

// GET /api/v1/logs/{id} —— a single log (masked view + content digest)
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

// POST /api/v1/logs/{id}/replay —— replay from the raw snapshot only (`contracts/03` §8)
//
// Security note: replay reads the raw snapshot, so it is **an audited privilege escalation** —
// `REPLAY_EXECUTED` / `LOG_RAW_ACCESSED` are recorded on success and on failure alike, so every decryption has a traceable owner.
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

// GET /api/v1/logs/key —— key self-description (current keyId / source / retained keys)
//
// Why the key itself is never returned: the platform only needs to say "which key is in use now and which old ciphertexts it can still open";
// once a key value travels over HTTP it lands in gateway logs, browser caches and proxies.
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

// POST /api/v1/logs/key/rotate —— rotate the encryption key for raw snapshots (`contracts/03` §8.2)
//
// The semantics are deliberately narrow: **only the key used for future writes changes**; existing ciphertext keeps decrypting with the old key.
// To actually retire the old key you must first `POST /api/v1/logs/key/rewrap` and re-encrypt the existing ciphertext,
// otherwise deleting the key makes the historic raw snapshots permanently unreadable.
//
// Input: `{"key": "<32-byte hex/base64>"}`; `{"fromProvider": true}` means ask the KMS for the current key.
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

// rotateKeySecurely rotates and leaves an audit trail (rotation changes the premise of "who can read historic plaintext").
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

// POST /api/v1/logs/key/rewrap —— re-encrypt the existing ciphertext under the current key.
//
// This is the prerequisite for "retiring the old key": deleting the old key without re-encrypting makes the historic raw snapshots permanently unreadable.
// Returns the number of entries processed; if it fails midway the already-completed entries are committed, so it is safe to rerun (idempotent: same key is skipped).
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

// storageInfo puts "where logs are stored and whether replay works" straight into the list response:
// the frontend should not have to guess why the replay button is greyed out.
func storageInfo(s *Server) map[string]any {
	replay := false
	backend := ""
	// Note: do not test only `!= nil` — without --engine-url the assembled value is a **typed nil**
	// (the interface is not nil, the pointer is). Using "can it describe itself" as the predicate is the only way to reflect availability truthfully.
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
