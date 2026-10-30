package obs

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"goldenrules/platform/persist"
)

// DefaultRetentionDays 原始快照默认保留期（`contracts/03` §8.2：默认 90 天）。
const DefaultRetentionDays = 90

// Service 决策日志服务：入库、查询、保留期清理、回放。
type Service struct {
	logs *persist.Store[LogDoc]
	raw  *persist.Store[RawDoc]
	box  *Box
	// retentionDays 原始快照保留期；<=0 表示「不留原始快照」（只存脱敏视图）。
	retentionDays int
	now           func() time.Time
}

// New 构造日志服务。`box` 可为 nil：此时拒收原始快照，只保留脱敏视图。
func New(logStore *persist.Store[LogDoc], rawStore *persist.Store[RawDoc], box *Box) *Service {
	return &Service{
		logs:          logStore,
		raw:           rawStore,
		box:           box,
		retentionDays: DefaultRetentionDays,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// WithClock 注入时钟（测试确定性，也用于验证保留期）。
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// WithRetention 设置原始快照保留天数；<=0 表示不留原始快照。
func (s *Service) WithRetention(days int) *Service {
	s.retentionDays = days
	return s
}

// RetentionDays 当前保留期。
func (s *Service) RetentionDays() int { return s.retentionDays }

// RawEnabled 是否接受原始快照（有密钥且保留期 > 0）。
func (s *Service) RawEnabled() bool { return s.box != nil && s.retentionDays > 0 }

// IngestResult 入库结果。
type IngestResult struct {
	Accepted   int      `json:"accepted"`
	Duplicate  int      `json:"duplicate"`
	RawStored  int      `json:"rawStored"`
	RawSkipped int      `json:"rawSkipped"`
	Purged     int      `json:"purgedRaw"`
	Warnings   []string `json:"warnings"`
	LogIDs     []string `json:"logIds"`
}

// Ingest 批量入库。幂等：同一 snapshotId+traceId 重复推送只记一次。
//
// 原始快照是可选件：没密钥 / 保留期为 0 时只存脱敏视图，并在响应里说明原因，
// 让调用方知道「这批日志以后回放不了」——而不是等到真要回放才发现。
func (s *Service) Ingest(items []IngestItem) (IngestResult, error) {
	out := IngestResult{Warnings: []string{}, LogIDs: []string{}}
	if len(items) == 0 {
		return out, errf(CodeInvalidLog, "items 为空")
	}
	if len(items) > 500 {
		return out, errf(CodeInvalidLog, "单批最多 500 条")
	}
	for i, it := range items {
		if err := it.Log.Validate(); err != nil {
			return out, errf(CodeInvalidLog, fmt.Sprintf("第 %d 条：%v", i+1, err))
		}
	}

	if !s.RawEnabled() && hasRaw(items) {
		out.Warnings = append(out.Warnings, rawDisabledReason(s))
	}

	now := s.now()
	purged, err := s.PurgeExpired()
	if err != nil {
		return out, err
	}
	out.Purged = purged

	err = s.logs.Update(func(d *LogDoc) error {
		if d.Seq == nil {
			d.Seq = map[string]int{}
		}
		seen := make(map[string]bool, len(d.Logs))
		for _, l := range d.Logs {
			seen[l.Entry.Key()] = true
		}
		for _, it := range items {
			if seen[it.Log.Key()] {
				out.Duplicate++
				continue
			}
			day := dayKey(now)
			d.Seq[day]++
			logID := fmt.Sprintf("LOG-%s-%04d", day, d.Seq[day])

			rec := StoredLog{
				LogID:       logID,
				Entry:       it.Log,
				ContentHash: it.Log.Hash(),
				ReceivedAt:  nowString(now),
			}
			if len(it.RawSnapshot) > 0 && s.RawEnabled() {
				ref, err := s.sealRaw(logID, it.RawSnapshot, now)
				if err != nil {
					return err
				}
				rec.Raw = ref
				out.RawStored++
			} else if len(it.RawSnapshot) > 0 {
				out.RawSkipped++
			}
			d.Logs = append(d.Logs, rec)
			out.Accepted++
			out.LogIDs = append(out.LogIDs, logID)
		}
		return nil
	})
	return out, err
}

// sealRaw 加密并写入 raw 文档。密文与日志视图分离存放（`contracts/03` §8.2）。
func (s *Service) sealRaw(logID string, plain []byte, now time.Time) (*RawSnapshotRef, error) {
	nonce, ct, err := s.box.Seal(plain, logID)
	if err != nil {
		return nil, err
	}
	expires := now.AddDate(0, 0, s.retentionDays)
	ref := RawSnapshotRef{
		LogID:      logID,
		Algo:       "AES-256-GCM",
		KeyID:      s.box.KeyID(),
		Nonce:      nonce,
		ExpiresAt:  nowString(expires),
		SizeBytes:  len(plain),
		Ciphertext: ct,
	}
	err = s.raw.Update(func(d *RawDoc) error {
		if d.Blobs == nil {
			d.Blobs = map[string]RawSnapshotRef{}
		}
		d.Blobs[logID] = ref
		return nil
	})
	if err != nil {
		return nil, err
	}
	// logs 文档只留引用元数据，密文挪走
	ref.Ciphertext = ""
	return &ref, nil
}

// PurgeExpired 清理超过保留期的原始快照（脱敏日志不动）。
func (s *Service) PurgeExpired() (int, error) {
	if s.box == nil {
		return 0, nil
	}
	now := s.now()
	purged := 0
	err := s.raw.Update(func(d *RawDoc) error {
		for id, b := range d.Blobs {
			exp, err := time.Parse(time.RFC3339, b.ExpiresAt)
			if err != nil || now.Before(exp) {
				continue
			}
			delete(d.Blobs, id)
			purged++
		}
		return nil
	})
	if purged > 0 {
		// 把日志上的引用标成已过期：回放时会给出明确原因，而不是「找不到」。
		_ = s.logs.Update(func(d *LogDoc) error {
			for i := range d.Logs {
				if d.Logs[i].Raw != nil && d.Logs[i].Raw.ExpiresAt != "" {
					exp, err := time.Parse(time.RFC3339, d.Logs[i].Raw.ExpiresAt)
					if err == nil && !now.Before(exp) {
						d.Logs[i].Raw.Ciphertext = ""
						d.Logs[i].Raw.SizeBytes = 0
					}
				}
			}
			return nil
		})
	}
	return purged, err
}

// Query 查询脱敏视图（分页）。返回命中列表与总数。
func (s *Service) Query(q Query) ([]StoredLog, int) {
	var hit []StoredLog
	s.logs.View(func(d *LogDoc) {
		for _, l := range d.Logs {
			if q.filter(l.Entry) {
				hit = append(hit, l)
			}
		}
	})
	sortByTimeDesc(hit)
	total := len(hit)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []StoredLog{}, total
	}
	hit = hit[offset:]
	if q.Limit > 0 && q.Limit < len(hit) {
		hit = hit[:q.Limit]
	}
	return hit, total
}

// Get 取一条日志（脱敏视图）。
func (s *Service) Get(logID string) (StoredLog, bool) {
	var out StoredLog
	var ok bool
	s.logs.View(func(d *LogDoc) {
		for _, l := range d.Logs {
			if l.LogID == logID {
				out, ok = l, true
				return
			}
		}
	})
	return out, ok
}

// Stats 汇总（观测面板用）：按决策分布、降级率、耗时分位。
func (s *Service) Stats(tenantID string) map[string]any {
	var (
		total    int
		degraded int
		byDec    = map[string]int{}
		byScene  = map[string]int{}
		totalMs  float64
		maxMs    float64
	)
	s.logs.View(func(d *LogDoc) {
		for _, l := range d.Logs {
			if tenantID != "" && l.Entry.TenantID != tenantID {
				continue
			}
			total++
			byDec[l.Entry.Decision]++
			byScene[l.Entry.Scene]++
			totalMs += l.Entry.ExecutionTimeMs
			if l.Entry.ExecutionTimeMs > maxMs {
				maxMs = l.Entry.ExecutionTimeMs
			}
			if l.Entry.Degraded {
				degraded++
			}
		}
	})
	avg := 0.0
	if total > 0 {
		avg = totalMs / float64(total)
	}
	return map[string]any{
		"total":         total,
		"degraded":      degraded,
		"byDecision":    byDec,
		"byScene":       byScene,
		"avgLatencyMs":  avg,
		"maxLatencyMs":  maxMs,
		"retentionDays": s.retentionDays,
		"rawEnabled":    s.RawEnabled(),
	}
}

// RawSnapshot 解密取原始快照。**只有回放通道可以调用**，调用方需记审计。
func (s *Service) RawSnapshot(logID string) (json.RawMessage, *Error) {
	if s.box == nil {
		return nil, errf(CodeKeyMissing, "未配置加密密钥，原始快照不可读")
	}
	rec, ok := s.Get(logID)
	if !ok {
		return nil, errf(CodeLogNotFound, "日志不存在: "+logID)
	}
	if rec.Raw == nil {
		return nil, errf(CodeRawUnavailable, "该日志入库时未保存原始快照（仅脱敏视图）")
	}
	if exp, err := time.Parse(time.RFC3339, rec.Raw.ExpiresAt); err == nil && !s.now().Before(exp) {
		return nil, errf(CodeRawPurged, "原始快照已过保留期（"+rec.Raw.ExpiresAt+"），不可回放")
	}
	var blob RawSnapshotRef
	var found bool
	s.raw.View(func(d *RawDoc) {
		blob, found = d.Blobs[logID]
	})
	if !found || strings.TrimSpace(blob.Ciphertext) == "" {
		return nil, errf(CodeRawPurged, "原始快照密文已不存在（可能已过期清理）")
	}
	// 按密文自述的 keyID 选钥：轮换后老密文仍用旧钥解，新密文用新钥解。
	// blob.KeyID 为空是历史数据（轮换前写入），此时 OpenWithKeyID 会依次尝试全部钥。
	plain, err := s.box.OpenWithKeyID(blob.Nonce, blob.Ciphertext, logID, blob.KeyID)
	if err != nil {
		return nil, &Error{Code: CodeDecryptFailed, Message: err.Error()}
	}
	return json.RawMessage(plain), nil
}

// ---------------------------------------------------------------------------
// 密钥轮换与 KMS 托管（`contracts/03` §8.2）

// KeyInfo 密钥自述（运维面板 / 轮换前后核对用）。
type KeyInfo struct {
	KeyID     string   `json:"keyId"`
	Source    string   `json:"source"`
	Provider  string   `json:"provider,omitempty"`
	Retained  []string `json:"retainedKeyIds"`
	RawEnable bool     `json:"rawEnabled"`
	Algo      string   `json:"algo"`
}

// KeyInfo 当前密钥状态。没配密钥时返回 nil（明确「没有」而不是给个空壳）。
func (s *Service) KeyInfo() *KeyInfo {
	if s.box == nil {
		return nil
	}
	return &KeyInfo{
		KeyID:     s.box.KeyID(),
		Source:    s.box.KeySource(),
		Provider:  s.box.ProviderName(),
		Retained:  s.box.RetainedKeyIDs(),
		RawEnable: s.RawEnabled(),
		Algo:      "AES-256-GCM",
	}
}

// RotateKey 轮换到显式给定的密钥。返回轮换后的 keyID。
//
// 只影响「此后新写入的密文用哪把钥匙」，历史密文继续用旧钥解密。
func (s *Service) RotateKey(encoded string) (string, *Error) {
	if s.box == nil {
		return "", errf(CodeKeyMissing, "未配置加密密钥，无可轮换对象")
	}
	if err := s.box.Rotate(encoded); err != nil {
		return "", asOBS(err)
	}
	return s.box.KeyID(), nil
}

// RotateKeyFromProvider 从托管源（KMS）拉当前钥并轮换。
func (s *Service) RotateKeyFromProvider() (string, *Error) {
	if s.box == nil {
		return "", errf(CodeKeyMissing, "未配置加密密钥，无可轮换对象")
	}
	id, err := s.box.RotateFromProvider()
	if err != nil {
		return "", asOBS(err)
	}
	return id, nil
}

// WithKeyProvider 挂托管密钥源。
func (s *Service) WithKeyProvider(p KeyProvider) *Service {
	if s.box != nil {
		s.box.WithKeyProvider(p)
	}
	return s
}

// RewrapRaw 把历史密文重加密到当前钥（下线旧钥的前置动作）。
//
// 为什么必须有这个：不重加密就删旧钥 = 历史原始快照永久不可读。
// 本函数逐条解旧密文、用当前钥重封，任一条失败即止并返回已处理条数——
// 半途失败时旧密文仍在（我们只在单条成功后覆盖），可安全重跑。
func (s *Service) RewrapRaw() (int, *Error) {
	if s.box == nil {
		return 0, errf(CodeKeyMissing, "未配置加密密钥，无可重加密对象")
	}
	cur := s.box.KeyID()
	done := 0
	var firstErr *Error
	err := s.raw.Update(func(d *RawDoc) error {
		for id, blob := range d.Blobs {
			if blob.KeyID == cur || strings.TrimSpace(blob.Ciphertext) == "" {
				continue
			}
			plain, oerr := s.box.OpenWithKeyID(blob.Nonce, blob.Ciphertext, id, blob.KeyID)
			if oerr != nil {
				firstErr = asOBS(oerr)
				return oerr
			}
			nonce, ct, serr := s.box.Seal(plain, id)
			if serr != nil {
				firstErr = asOBS(serr)
				return serr
			}
			blob.Nonce, blob.Ciphertext, blob.KeyID = nonce, ct, cur
			d.Blobs[id] = blob
			done++
		}
		return nil
	})
	if firstErr != nil {
		return done, firstErr
	}
	if err != nil {
		return done, asOBS(err)
	}
	return done, nil
}

// ---------------------------------------------------------------------------

func hasRaw(items []IngestItem) bool {
	for _, it := range items {
		if len(it.RawSnapshot) > 0 {
			return true
		}
	}
	return false
}

func rawDisabledReason(s *Service) string {
	if s.box == nil {
		return "未配置加密密钥，本次只保存脱敏视图（回放不可用）"
	}
	return "原始快照保留期为 0，本次只保存脱敏视图（回放不可用）"
}
