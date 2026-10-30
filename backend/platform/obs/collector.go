package obs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Collector is the log collection sidecar (F-OBS06): it moves the engine's structured decision logs from stdout into the platform.
//
// Why a sidecar instead of "the engine writes to the platform directly": `contracts/03` §8 requires the decision path to be **independent** of
// whether log storage is available. The engine just writes one JSON line to stdout, so collection failures and platform restarts never affect decisions;
// conversely, having the engine report over synchronous HTTP on the decision path makes every platform hiccup a decision P99 hiccup.
//
// Three deliberate trade-offs:
//
//  1. **Drop when the buffer is full, never block**. Once the sidecar stalls, the stdout pipe fills up and eventually blocks the engine —
//     that is "logging killing decisions", far worse than losing logs. Dropped counts are tracked and reported periodically, never silenced.
//  2. **Noise lines are skipped outright**. The engine also prints a human-readable banner to stdout at startup, and the sidecar must not
//     exit just because one line is not JSON.
//  3. **Retries are bounded**. Retrying forever just exhausts memory; once retries run out the whole batch is recorded as failed and we move on,
//     so "the platform is down" shows up as an observable failure count instead of the sidecar's memory quietly growing until OOM.
type Collector struct {
	cfg    CollectorConfig
	client IngestClient

	buf []LogEntry
	// warn is an optional warning hook (wired into logging by cmd). The sidecar itself does not print,
	// otherwise tests would be flooded and the library would be hard to reuse.
	warn func(string)
	// The counters only increase, so ops can see "how much was dropped today".
	received atomic.Uint64
	dropped  atomic.Uint64
	sent     atomic.Uint64
	failed   atomic.Uint64
	noise    atomic.Uint64
	batches  atomic.Uint64
}

// IngestClient pushes a batch of logs to the platform. It is an interface so tests do not have to run an HTTP server.
type IngestClient interface {
	Push(items []IngestItem) (IngestResult, error)
	Describe() string
}

// CollectorConfig holds the collection parameters.
type CollectorConfig struct {
	// Batch is the number of entries per batch (the platform caps a batch at 500)
	Batch int
	// Flush is the maximum wait when a batch is not yet full (it sets the log latency ceiling)
	Flush time.Duration
	// Buffer is the buffer cap; 0 means never drop (dangerous: it makes the engine wait for the logs)
	Buffer int
	// Retries is the maximum number of retries per batch
	Retries int
	// Backoff is the first backoff duration (grows exponentially, capped at 5s)
	Backoff time.Duration
}

// DefaultCollectorConfig holds the defaults: 200 entries per batch, a flush every 200ms, a 10k buffer.
func DefaultCollectorConfig() CollectorConfig {
	return CollectorConfig{
		Batch:   200,
		Flush:   200 * time.Millisecond,
		Buffer:  10000,
		Retries: 5,
		Backoff: 100 * time.Millisecond,
	}
}

// CollectorStats holds collection statistics (periodically written to stderr, final values on exit).
type CollectorStats struct {
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
	Failed   uint64 `json:"failed"`
	Dropped  uint64 `json:"dropped"`
	Noise    uint64 `json:"noiseLines"`
	Batches  uint64 `json:"batches"`
	Pending  int    `json:"pending"`
}

// NewCollector builds the sidecar collector.
func NewCollector(cfg CollectorConfig, client IngestClient) *Collector {
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultCollectorConfig().Batch
	}
	if cfg.Batch > 500 {
		cfg.Batch = 500 // hard per-batch cap on the platform; an oversized batch is rejected whole
	}
	if cfg.Flush <= 0 {
		cfg.Flush = DefaultCollectorConfig().Flush
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = DefaultCollectorConfig().Backoff
	}
	return &Collector{cfg: cfg, client: client}
}

// WithWarn attaches the warning hook (the first observation point for push failures/drops).
func (c *Collector) WithWarn(fn func(string)) *Collector {
	c.warn = fn
	return c
}

func (c *Collector) warnf(format string, args ...any) {
	if c.warn != nil {
		c.warn(fmt.Sprintf(format, args...))
	}
}

// Stats returns the current statistics.
func (c *Collector) Stats() CollectorStats {
	return CollectorStats{
		Received: c.received.Load(),
		Sent:     c.sent.Load(),
		Failed:   c.failed.Load(),
		Dropped:  c.dropped.Load(),
		Noise:    c.noise.Load(),
		Batches:  c.batches.Load(),
		Pending:  len(c.buf),
	}
}

// Run reads from `src` until EOF or the stop channel closes; it flushes the remaining logs before returning.
//
// The returned error only means "reading the stream failed" and never a push failure — push failures are already counted in the stats,
// and letting the sidecar exit because the platform is temporarily unavailable turns "a recoverable upstream fault" into "collection permanently stopped".
func (c *Collector) Run(src io.Reader, stop <-chan struct{}) error {
	scanner := bufio.NewScanner(src)
	// A single log line may carry a large redacted snapshot, so the default 64KB limit is not enough
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lastFlush := time.Now()
	for {
		select {
		case <-stop:
			c.Flush()
			return nil
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				c.Flush()
				return fmt.Errorf("读取日志流失败: %w", err)
			}
			c.Flush()
			return nil
		}

		c.Accept(scanner.Bytes())

		if len(c.buf) >= c.cfg.Batch || time.Since(lastFlush) >= c.cfg.Flush {
			c.Flush()
			lastFlush = time.Now()
		}
	}
}

// Accept processes one input line: a decision log goes into the buffer, anything else is skipped as noise.
//
// The criterion is "can it be parsed into a valid LogEntry", not "does it start with {":
// the engine's startup banner may contain braces too, while a JSON parse failure by itself already proves the line is not a log.
func (c *Collector) Accept(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		c.noise.Add(1)
		return
	}
	var entry LogEntry
	if err := json.Unmarshal(trimmed, &entry); err != nil {
		c.noise.Add(1)
		return
	}
	// Missing required fields mean this is not a decision log (or the engine version is incompatible); treat it as noise and count it,
	// but never drop it silently — otherwise "there are fewer logs" becomes unexplainable.
	if err := entry.Validate(); err != nil {
		c.noise.Add(1)
		return
	}
	c.received.Add(1)

	if c.cfg.Buffer > 0 && len(c.buf) >= c.cfg.Buffer {
		// Drop the newest and count it: the earlier logs already in the buffer are more valuable for incident review
		if n := c.dropped.Add(1); n == 1 || n%1000 == 0 {
			c.warnf("缓冲已满（%d 条），累计丢弃 %d 条日志；请检查平台可用性", c.cfg.Buffer, n)
		}
		return
	}
	c.buf = append(c.buf, entry)
}

// Flush pushes the buffer to the platform (with retries). It returns the number of entries pushed successfully.
func (c *Collector) Flush() int {
	if len(c.buf) == 0 {
		return 0
	}
	items := make([]IngestItem, 0, len(c.buf))
	for _, e := range c.buf {
		items = append(items, IngestItem{Log: e})
	}
	c.buf = c.buf[:0]

	backoff := c.cfg.Backoff
	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		_, err := c.client.Push(items)
		if err == nil {
			c.sent.Add(uint64(len(items)))
			c.batches.Add(1)
			return len(items)
		}
		lastErr = err
		// When the platform explicitly says "this batch is bad" (4xx), retrying is pointless; record the failure immediately
		var nerr *permanentError
		if errors.As(err, &nerr) {
			break
		}
		if attempt < c.cfg.Retries {
			time.Sleep(backoff)
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}
	c.failed.Add(uint64(len(items)))
	c.warnf("推送 %d 条日志失败（已重试 %d 次）：%v", len(items), c.cfg.Retries, lastErr)
	return 0
}

// ---------------------------------------------------------------- HTTP client

// permanentError marks a failure where retrying is pointless (4xx).
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

// HTTPIngestClient pushes logs to the platform via `POST /api/v1/logs`.
type HTTPIngestClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPIngestClient builds the push client.
func NewHTTPIngestClient(baseURL string) *HTTPIngestClient {
	return &HTTPIngestClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (h *HTTPIngestClient) Describe() string { return "platform:" + h.baseURL + " /api/v1/logs" }

// Push pushes one batch of logs.
func (h *HTTPIngestClient) Push(items []IngestItem) (IngestResult, error) {
	var out IngestResult
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return out, &permanentError{msg: "序列化日志失败: " + err.Error()}
	}
	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/logs", bytes.NewReader(body))
	if err != nil {
		return out, &permanentError{msg: "构造请求失败: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return out, err // network errors are retryable
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, &permanentError{msg: fmt.Sprintf("平台拒绝（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	if resp.StatusCode >= 500 {
		return out, fmt.Errorf("平台不可用（HTTP %d）", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("解析平台响应失败: %w", err)
	}
	return out, nil
}
