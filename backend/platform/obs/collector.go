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

// Collector 日志采集侧车（F-OBS06）：把引擎 stdout 的结构化决策日志搬进平台。
//
// 为什么要有侧车而不是「引擎直接写平台」：`contracts/03` §8 要求决策路径**不依赖**
// 日志存储是否可用。引擎只管往 stdout 写一行 JSON，采集失败、平台重启都不影响决策；
// 反过来，让引擎在决策路径上同步 HTTP 上报，平台抖一下就是决策 P99 抖一下。
//
// 三条刻意的取舍：
//
//  1. **缓冲满就丢，不阻塞**。侧车一旦卡住，stdout 的管道会被填满，最终把引擎阻塞——
//     那是「日志拖死决策」，比丢日志严重得多。丢的条数计入统计并周期上报，绝不静默。
//  2. **噪声行直接跳过**。引擎启动时也会往 stdout 打人类可读的横幅，侧车不能因为
//     一行不是 JSON 就退出。
//  3. **重试有上限**。无限重试等于把内存耗光；重试用尽后整批记失败并继续前进，
//     让「平台挂了」表现为可观测的失败计数，而不是侧车内存悄悄涨到 OOM。
type Collector struct {
	cfg    CollectorConfig
	client IngestClient

	buf []LogEntry
	// warn 可选告警钩子（由 cmd 接到日志里）。侧车本身不打印，
	// 否则测试里会刷屏、库也不好复用。
	warn func(string)
	// 统计只增不减，便于运维看「今天丢了多少」。
	received atomic.Uint64
	dropped  atomic.Uint64
	sent     atomic.Uint64
	failed   atomic.Uint64
	noise    atomic.Uint64
	batches  atomic.Uint64
}

// IngestClient 把一批日志推给平台。抽成接口是为了让测试不必起 HTTP 服务。
type IngestClient interface {
	Push(items []IngestItem) (IngestResult, error)
	Describe() string
}

// CollectorConfig 采集参数。
type CollectorConfig struct {
	// Batch 每批条数（平台单批上限 500）
	Batch int
	// Flush 攒不够一批时的最长等待（决定日志延迟上限）
	Flush time.Duration
	// Buffer 缓冲上限；0 表示不丢（危险：等于让引擎等日志）
	Buffer int
	// Retries 单批最多重试次数
	Retries int
	// Backoff 首次退避时长（指数增长，上限 5s）
	Backoff time.Duration
}

// DefaultCollectorConfig 默认参数：200 条一批、200ms 一刷、1 万条缓冲。
func DefaultCollectorConfig() CollectorConfig {
	return CollectorConfig{
		Batch:   200,
		Flush:   200 * time.Millisecond,
		Buffer:  10000,
		Retries: 5,
		Backoff: 100 * time.Millisecond,
	}
}

// CollectorStats 采集统计（周期打到 stderr，进程退出时打最终值）。
type CollectorStats struct {
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
	Failed   uint64 `json:"failed"`
	Dropped  uint64 `json:"dropped"`
	Noise    uint64 `json:"noiseLines"`
	Batches  uint64 `json:"batches"`
	Pending  int    `json:"pending"`
}

// NewCollector 构造侧车采集器。
func NewCollector(cfg CollectorConfig, client IngestClient) *Collector {
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultCollectorConfig().Batch
	}
	if cfg.Batch > 500 {
		cfg.Batch = 500 // 平台单批硬上限，超了整批会被拒
	}
	if cfg.Flush <= 0 {
		cfg.Flush = DefaultCollectorConfig().Flush
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = DefaultCollectorConfig().Backoff
	}
	return &Collector{cfg: cfg, client: client}
}

// WithWarn 挂告警钩子（推送失败/丢弃时的第一现场）。
func (c *Collector) WithWarn(fn func(string)) *Collector {
	c.warn = fn
	return c
}

func (c *Collector) warnf(format string, args ...any) {
	if c.warn != nil {
		c.warn(fmt.Sprintf(format, args...))
	}
}

// Stats 当前统计。
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

// Run 读 `src` 直到 EOF 或 ctx 取消；退出前把剩余日志刷出去。
//
// 返回的 error 只表示「读流失败」，不含推送失败——推送失败已计入统计，
// 让侧车因为平台暂时不可用而退出，会把「可恢复的上游故障」变成「采集永久停摆」。
func (c *Collector) Run(src io.Reader, stop <-chan struct{}) error {
	scanner := bufio.NewScanner(src)
	// 单条日志可能带较大的脱敏快照，默认 64KB 上限不够
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

// Accept 处理一行输入：是决策日志就入缓冲，不是就当噪声跳过。
//
// 判据是「能否解析成合法 LogEntry」而不是「是不是以 { 开头」：
// 引擎的启动横幅里也可能有花括号，而 JSON 解析失败本身就说明这行不是日志。
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
	// 必填字段缺失说明这不是决策日志（或引擎版本不兼容），按噪声处理并计数，
	// 不要静默丢——否则「日志变少了」会查不出原因。
	if err := entry.Validate(); err != nil {
		c.noise.Add(1)
		return
	}
	c.received.Add(1)

	if c.cfg.Buffer > 0 && len(c.buf) >= c.cfg.Buffer {
		// 丢最新的并计数：缓冲里已有的更早日志对事故复盘更有价值
		if n := c.dropped.Add(1); n == 1 || n%1000 == 0 {
			c.warnf("缓冲已满（%d 条），累计丢弃 %d 条日志；请检查平台可用性", c.cfg.Buffer, n)
		}
		return
	}
	c.buf = append(c.buf, entry)
}

// Flush 把缓冲推给平台（含重试）。返回成功推送的条数。
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
		// 平台明确说「这批有问题」（4xx）时重试没有意义，直接记失败
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

// ---------------------------------------------------------------- HTTP 客户端

// permanentError 标记「重试也没用」的失败（4xx）。
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

// HTTPIngestClient 把日志推给平台 `POST /api/v1/logs`。
type HTTPIngestClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPIngestClient 构造推送客户端。
func NewHTTPIngestClient(baseURL string) *HTTPIngestClient {
	return &HTTPIngestClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (h *HTTPIngestClient) Describe() string { return "platform:" + h.baseURL + " /api/v1/logs" }

// Push 推送一批日志。
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
		return out, err // 网络错误可重试
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
