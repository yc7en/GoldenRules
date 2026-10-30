// logcollector is the decision log collection sidecar (F-OBS06): it moves the structured decision logs from the engine's stdout into the platform.
//
// Usage (simplest when co-located with the engine):
//
//	gr-server ... | logcollector --platform http://127.0.0.1:8080
//	logcollector --platform http://127.0.0.1:8080 --source /var/log/goldenrules/engine.log
//
// Why a pipe instead of letting the engine talk to the platform directly: the decision path must not depend on whether log storage is available
// (`contracts/03` §8). The pipe takes "log delivery" off the decision path — the platform can be down and
// decisions continue; the sidecar holds on in its own buffer, and when it cannot it drops and counts (see obs/collector.go).
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"goldenrules/platform/obs"
)

func main() {
	platform := flag.String("platform", "http://127.0.0.1:8080", "平台地址（POST /api/v1/logs）")
	source := flag.String("source", "-", "日志来源：- 表示 stdin，否则是文件路径（按行读取，读到 EOF 结束）")
	batch := flag.Int("batch", 200, "每批条数（平台单批上限 500）")
	flush := flag.Duration("flush", 200*time.Millisecond, "攒不满一批时的最长等待")
	buffer := flag.Int("buffer", 10000, "缓冲条数上限；满了丢最新并计数（0 表示不丢，等于让引擎等日志）")
	retries := flag.Int("retries", 5, "单批最多重试次数")
	statsEvery := flag.Duration("stats-interval", time.Minute, "统计上报间隔；0 表示只在退出时报一次")
	flag.Parse()

	var src io.Reader
	switch *source {
	case "-", "":
		src = os.Stdin
	default:
		f, err := os.Open(*source)
		if err != nil {
			log.Fatalf("打开日志来源失败: %v", err)
		}
		defer f.Close()
		src = f
	}

	// A buffer of 0 is a dangerous configuration: the read side blocks forever on push retries and eventually plugs the engine's pipe.
	if *buffer == 0 {
		log.Printf("警告：--buffer 0 表示不丢弃，平台长时间不可用会反压到引擎 stdout（可能拖慢决策）")
	}

	cfg := obs.DefaultCollectorConfig()
	cfg.Batch = *batch
	cfg.Flush = *flush
	cfg.Buffer = *buffer
	cfg.Retries = *retries

	client := obs.NewHTTPIngestClient(*platform)
	collector := obs.NewCollector(cfg, client).WithWarn(func(msg string) { log.Printf("警告：%s", msg) })

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Printf("收到 %s，停止采集并刷出剩余日志", sig)
		close(stop)
	}()

	if *statsEvery > 0 {
		go func() {
			t := time.NewTicker(*statsEvery)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					log.Printf("采集统计: %s", statsLine(collector.Stats()))
				}
			}
		}()
	}

	log.Printf("logcollector 启动：source=%s → %s", *source, client.Describe())
	if err := collector.Run(src, stop); err != nil {
		log.Printf("采集结束（异常）：%v", err)
	}
	log.Printf("采集结束: %s", statsLine(collector.Stats()))
}

// statsLine renders the stats on one line: how much was dropped must be visible at a glance every time.
func statsLine(s obs.CollectorStats) string {
	return fmt.Sprintf(
		"收到=%d 已推送=%d 失败=%d 丢弃=%d 噪声行=%d 批次=%d 待推送=%d",
		s.Received, s.Sent, s.Failed, s.Dropped, s.Noise, s.Batches, s.Pending,
	)
}
