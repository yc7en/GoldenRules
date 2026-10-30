package obs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClient is a controllable push endpoint: it records the batches it receives and returns errors by script.
type fakeClient struct {
	batches  [][]IngestItem
	errs     []error // consumed in order; empty means success
	calls    int
	describe string
}

func (f *fakeClient) Describe() string { return "fake" }

func (f *fakeClient) Push(items []IngestItem) (IngestResult, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return IngestResult{}, err
		}
	}
	f.batches = append(f.batches, append([]IngestItem(nil), items...))
	return IngestResult{Accepted: len(items)}, nil
}

func (f *fakeClient) total() int {
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

// Engine stdout carries both the startup banner and decision logs: the sidecar must take only the latter.
func TestCollectorSkipsNoiseAndKeepsDecisionLogs(t *testing.T) {
	f := &fakeClient{}
	c := NewCollector(DefaultCollectorConfig(), f)

	c.Accept([]byte("gr-server listening on http://127.0.0.1:8080（keep-alive 开启）"))
	c.Accept([]byte(""))
	c.Accept([]byte(`{"not":"a log"}`))
	c.Accept([]byte(`{ 坏的 JSON`))
	line := mustJSON(t, sampleLog("snap-1", "trace-1", "PASS"))
	c.Accept(line)

	s := c.Stats()
	if s.Received != 1 || s.Noise != 4 {
		t.Fatalf("应只收 1 条、跳过 4 条噪声，实际 %+v", s)
	}
	if got := c.Flush(); got != 1 {
		t.Fatalf("应推送 1 条，实际 %d", got)
	}
	if f.total() != 1 {
		t.Fatalf("客户端收到 %d 条", f.total())
	}
}

// Push as soon as a batch is full, do not keep accumulating — otherwise log latency depends on traffic.
func TestCollectorFlushesAtBatchSize(t *testing.T) {
	f := &fakeClient{}
	cfg := DefaultCollectorConfig()
	cfg.Batch = 3
	c := NewCollector(cfg, f)

	for i := 0; i < 3; i++ {
		c.Accept(mustJSON(t, sampleLog("snap", "trace", "PASS")))
	}
	if got := c.Flush(); got != 3 {
		t.Fatalf("应推送 3 条，实际 %d", got)
	}
	if len(f.batches) != 1 {
		t.Fatalf("应合成 1 批，实际 %d 批", len(f.batches))
	}
}

// Platform temporarily unavailable → succeeds after retries, no logs are lost.
func TestCollectorRetriesTransientFailure(t *testing.T) {
	f := &fakeClient{errs: []error{errors.New("connection refused"), errors.New("connection refused")}}
	cfg := DefaultCollectorConfig()
	cfg.Backoff = time.Millisecond
	c := NewCollector(cfg, f)

	c.Accept(mustJSON(t, sampleLog("snap", "trace", "PASS")))
	if got := c.Flush(); got != 1 {
		t.Fatalf("重试后应成功推送 1 条，实际 %d", got)
	}
	if f.calls != 3 {
		t.Fatalf("应尝试 3 次（1 + 2 次重试），实际 %d", f.calls)
	}
	if s := c.Stats(); s.Failed != 0 || s.Sent != 1 {
		t.Fatalf("统计不对: %+v", s)
	}
}

// 4xx (the batch itself is bad) is not retried: retrying would only hit the same error again.
func TestCollectorDoesNotRetryPermanentFailure(t *testing.T) {
	f := &fakeClient{errs: []error{&permanentError{msg: "平台拒绝（HTTP 422）"}}}
	c := NewCollector(DefaultCollectorConfig(), f)
	c.Accept(mustJSON(t, sampleLog("snap", "trace", "PASS")))
	if got := c.Flush(); got != 0 {
		t.Fatalf("永久失败不应算推送成功，实际 %d", got)
	}
	if f.calls != 1 {
		t.Fatalf("4xx 不应重试，实际调用 %d 次", f.calls)
	}
	if s := c.Stats(); s.Failed != 1 {
		t.Fatalf("失败应计入统计: %+v", s)
	}
}

// Buffer full → drop and count, never block (blocking would wedge the engine's stdout pipe).
func TestCollectorDropsWhenBufferFull(t *testing.T) {
	f := &fakeClient{}
	cfg := DefaultCollectorConfig()
	cfg.Buffer = 2
	c := NewCollector(cfg, f)

	for i := 0; i < 5; i++ {
		c.Accept(mustJSON(t, sampleLog("snap", "trace", "PASS")))
	}
	s := c.Stats()
	if s.Received != 5 || s.Dropped != 3 || s.Pending != 2 {
		t.Fatalf("应收到 5/丢 3/待推 2，实际 %+v", s)
	}
	if got := c.Flush(); got != 2 {
		t.Fatalf("只应推送缓冲里的 2 条，实际 %d", got)
	}
}

// Retries exhausted → the whole batch is recorded as failed and we move on (no unbounded memory growth).
func TestCollectorGivesUpAfterRetries(t *testing.T) {
	f := &fakeClient{errs: []error{
		errors.New("e1"), errors.New("e2"), errors.New("e3"), errors.New("e4"), errors.New("e5"), errors.New("e6"),
	}}
	cfg := DefaultCollectorConfig()
	cfg.Retries = 2
	cfg.Backoff = time.Millisecond
	c := NewCollector(cfg, f)
	c.Accept(mustJSON(t, sampleLog("snap", "trace", "PASS")))
	if got := c.Flush(); got != 0 {
		t.Fatalf("应放弃，实际推送 %d", got)
	}
	if f.calls != 3 {
		t.Fatalf("应尝试 1+2 次，实际 %d", f.calls)
	}
	if s := c.Stats(); s.Failed != 1 || s.Pending != 0 {
		t.Fatalf("失败后缓冲应清空: %+v", s)
	}
}

// Run flushes the remaining logs on EOF (when the pipe closes the last batch must not stay in memory).
func TestCollectorFlushesOnEOF(t *testing.T) {
	f := &fakeClient{}
	cfg := DefaultCollectorConfig()
	cfg.Batch = 100 // keep the batch from triggering, so only EOF flushes
	c := NewCollector(cfg, f)

	line := string(mustJSON(t, sampleLog("snap", "trace", "PASS"))) + "\n"
	err := c.Run(strings.NewReader(line+"gr-server listening\n"), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.total() != 1 {
		t.Fatalf("EOF 应刷出 1 条，实际 %d", f.total())
	}
	if s := c.Stats(); s.Noise != 1 {
		t.Fatalf("噪声行计数 = %d", s.Noise)
	}
}

// Batches over 500 entries are rejected wholesale by the platform: the collector must truncate to the cap itself.
func TestCollectorClampsBatchToPlatformLimit(t *testing.T) {
	cfg := DefaultCollectorConfig()
	cfg.Batch = 9999
	c := NewCollector(cfg, &fakeClient{})
	if c.cfg.Batch != 500 {
		t.Fatalf("批次应被压到 500，实际 %d", c.cfg.Batch)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
