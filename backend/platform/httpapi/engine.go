package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// 引擎转发：浏览器只跟平台说话，决策服务不对外开跨源。
//
// 为什么不让引擎自己开 CORS：引擎是无认证的内部服务，任何网页只要能跨源喊它，
// 就等于把「这台机器的决策能力」挂到公网上。平台本来就有 --cors-origin 这个
// 显式开关，转发放在这里，暴露面只有一个点、且是可控的那个。
//
// 代价是试跑多一跳。试跑是人在界面上点的动作，不在生产决策路径上，这一跳可以接受。

const engineTimeout = 5 * time.Second

// GET /api/v1/engine —— 引擎自述：活着没有、装的是什么包。
func (s *Server) engineInfo(w http.ResponseWriter, r *http.Request) {
	if s.deps.EngineURL == "" {
		writeError(w, r, http.StatusServiceUnavailable, "ENGINE_NOT_CONFIGURED",
			"平台没有配置 --engine-url，引擎信息与试跑不可用", nil)
		return
	}
	client := &http.Client{Timeout: engineTimeout}

	health, err := s.engineGet(client, r, "/healthz")
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "ENGINE_UNREACHABLE",
			"连不上决策服务："+err.Error(), nil)
		return
	}

	raw, err := s.engineGet(client, r, "/v1/packages")
	if err != nil {
		// 活着但报错：如实说「活着、但拿不到包清单」，不要合成一个 healthy=true 的假象
		writeJSON(w, http.StatusOK, map[string]any{
			"healthy": true,
			"baseUrl": s.deps.EngineURL,
			"health":  json.RawMessage(health),
			"error":   "拿不到已装载包清单：" + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"healthy":  true,
		"baseUrl":  s.deps.EngineURL,
		"health":   json.RawMessage(health),
		"packages": json.RawMessage(raw),
	})
}

// POST /api/v1/trial —— 原样转发到引擎 /v1/trial。
//
// 响应（含错误）逐字透传：试跑的诊断属于引擎，平台不该二次解释——
// 一旦在这里改写错误码，界面上看到的就不再是引擎真实说的话。
func (s *Server) engineTrial(w http.ResponseWriter, r *http.Request) {
	if s.deps.EngineURL == "" {
		writeError(w, r, http.StatusServiceUnavailable, "ENGINE_NOT_CONFIGURED",
			"平台没有配置 --engine-url，试跑不可用", nil)
		return
	}
	const maxBody = 4 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "读取请求体失败: "+err.Error(), nil)
		return
	}
	if len(body) > maxBody {
		// 截断转发比直接拒绝更糟：引擎会收到半截 JSON，报一个与真实原因无关的解析错。
		writeError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE",
			"试跑请求体超过 4MB，请精简事实快照", nil)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.deps.EngineURL+"/v1/trial", bytes.NewReader(body))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "ENGINE_REQUEST_FAILED", err.Error(), nil)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: engineTimeout}).Do(req)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "ENGINE_UNREACHABLE",
			"连不上决策服务："+err.Error(), nil)
		return
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "ENGINE_READ_FAILED", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

func (s *Server) engineGet(client *http.Client, r *http.Request, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.deps.EngineURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &engineStatusError{status: resp.StatusCode, body: string(body)}
	}
	return body, nil
}

type engineStatusError struct {
	status int
	body   string
}

func (e *engineStatusError) Error() string {
	msg := e.body
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return http.StatusText(e.status) + " " + msg
}
