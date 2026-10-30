package obs

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"goldenrules/platform/persist"
)

// DefaultRetentionDays is the default retention of raw snapshots (`contracts/03` §8.2: 90 days by default).
const DefaultRetentionDays = 90

// Service is the decision log service: ingest, query, retention purging, replay.
type Service struct {
	logs *persist.Store[LogDoc]
	raw  *persist.Store[RawDoc]
	box  *Box
	// retentionDays is the raw snapshot retention; <=0 means "keep no raw snapshots" (store the redacted view only).
	retentionDays int
	now           func() time.Time
}

// New builds the log service. `box` may be nil: raw snapshots are then refused and only the redacted view is kept.
func New(logStore *persist.Store[LogDoc], rawStore *persist.Store[RawDoc], box *Box) *Service {
	return &Service{
		logs:          logStore,
		raw:           rawStore,
		box:           box,
		retentionDays: DefaultRetentionDays,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// WithClock injects the clock (deterministic tests, also used to verify retention).
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

// WithRetention sets the raw snapshot retention in days; <=0 means keep no raw snapshots.
func (s *Service) WithRetention(days int) *Service {
	s.retentionDays = days
	return s
}

// RetentionDays is the current retention.
func (s *Service) RetentionDays() int { return s.retentionDays }

// RawEnabled reports whether raw snapshots are accepted (a key is configured and retention > 0).
func (s *Service) RawEnabled() bool { return s.box != nil && s.retentionDays > 0 }

// IngestResult is the ingest result.
type IngestResult struct {
	Accepted   int      `json:"accepted"`
	Duplicate  int      `json:"duplicate"`
	RawStored  int      `json:"rawStored"`
	RawSkipped int      `json:"rawSkipped"`
	Purged     int      `json:"purgedRaw"`
	Warnings   []string `json:"warnings"`
	LogIDs     []string `json:"logIds"`
}

// Ingest stores a batch. Idempotent: pushing the same snapshotId+traceId again is recorded only once.
//
// The raw snapshot is optional: with no key / with retention 0 only the redacted view is stored, and the response says why,
// so the caller knows "this batch cannot be replayed later" — instead of finding out only when a replay is actually needed.
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

// sealRaw encrypts and writes into the raw document. Ciphertext and log view are stored separately (`contracts/03` §8.2).
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
	// the logs document keeps only the reference metadata; the ciphertext is moved away
	ref.Ciphertext = ""
	return &ref, nil
}

// PurgeExpired purges raw snapshots past their retention (redacted logs are untouched).
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
		// mark the reference on the log as expired: a replay then gives a clear reason instead of "not found".
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

// Query queries the redacted view (paged). It returns the matching list and the total count.
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

// Get fetches one log (redacted view).
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

// Stats aggregates (for the observability panel): distribution by decision, degrade rate, latency percentiles.
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

// RawSnapshot decrypts and returns the raw snapshot. **Only the replay channel may call this**; callers must record an audit trail.
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
	// Pick the key by the keyID the ciphertext declares: after rotation old ciphertext is still decrypted with the old key, new ciphertext with the new key.
	// An empty blob.KeyID means historical data (written before rotation); OpenWithKeyID then tries every key in turn.
	plain, err := s.box.OpenWithKeyID(blob.Nonce, blob.Ciphertext, logID, blob.KeyID)
	if err != nil {
		return nil, &Error{Code: CodeDecryptFailed, Message: err.Error()}
	}
	return json.RawMessage(plain), nil
}

// ---------------------------------------------------------------------------
// Key rotation and KMS hosting (`contracts/03` §8.2)

// KeyInfo is the key self-description (for the ops panel / before-and-after rotation checks).
type KeyInfo struct {
	KeyID     string   `json:"keyId"`
	Source    string   `json:"source"`
	Provider  string   `json:"provider,omitempty"`
	Retained  []string `json:"retainedKeyIds"`
	RawEnable bool     `json:"rawEnabled"`
	Algo      string   `json:"algo"`
}

// KeyInfo returns the current key status. It returns nil when no key is configured (an explicit "none" rather than an empty shell).
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

// RotateKey rotates to the explicitly given key. It returns the keyID after rotation.
//
// It only affects "which key later writes use"; historical ciphertext keeps being decrypted with the old key.
func (s *Service) RotateKey(encoded string) (string, *Error) {
	if s.box == nil {
		return "", errf(CodeKeyMissing, "未配置加密密钥，无可轮换对象")
	}
	if err := s.box.Rotate(encoded); err != nil {
		return "", asOBS(err)
	}
	return s.box.KeyID(), nil
}

// RotateKeyFromProvider pulls the current key from the managed source (KMS) and rotates to it.
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

// WithKeyProvider attaches the managed key source.
func (s *Service) WithKeyProvider(p KeyProvider) *Service {
	if s.box != nil {
		s.box.WithKeyProvider(p)
	}
	return s
}

// RewrapRaw re-encrypts historical ciphertext under the current key (the prerequisite for retiring an old key).
//
// Why this is mandatory: deleting the old key without re-encrypting = historical raw snapshots become permanently unreadable.
// This function decrypts each old ciphertext and re-seals it with the current key; the first failure stops it and it returns the count already processed —
// a mid-way failure leaves the old ciphertext in place (we only overwrite after a single entry succeeds), so it is safe to re-run.
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
