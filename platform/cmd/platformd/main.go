// Command platformd 是 BRMS 管理平台（Go）的进程骨架。
//
// B0 只保证：进程能独立起来、契约绑定能编译、契约 JSON 能双向解析。
// B6 起在本进程内填事实目录 / 规则编辑 / 校验 / 审批 / 发布，决策执行仍在 Rust 决策服务。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"goldenrules/platform/contracts"
)

func main() {
	addr := flag.String("addr", ":8080", "管理平台监听地址")
	flag.Parse()

	mux := http.NewServeMux()

	// 健康检查：只报进程活着 + 契约版本，不碰决策服务
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"service":    "goldenrules-platform",
			"timeLayout": contracts.GoldenTimeLayout,
			"now":        time.Now().UTC().Format(time.RFC3339),
		})
	})

	// B6+ 的接口占位：契约在 contracts/ 里已经冻结，实现按 PLAN 包推进
	mux.HandleFunc("GET /api/v1/facts", notImplemented("B6"))
	mux.HandleFunc("GET /api/v1/rulesets", notImplemented("B7"))
	mux.HandleFunc("GET /api/v1/packages", notImplemented("B8"))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("goldenrules-platform listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("platform exit: %v", err)
	}
}

func notImplemented(pkg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, contracts.ErrorContract{
			ErrorCode: "NOT_IMPLEMENTED",
			Message:   "该能力属于 " + pkg + " 工作包，尚未实现",
			TraceID:   r.Header.Get("X-Trace-Id"),
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
