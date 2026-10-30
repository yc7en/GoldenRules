package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Engine forwarding: the browser only talks to the platform; the decision service never enables CORS for the outside.
//
// Why not let the engine enable CORS itself: the engine is an unauthenticated internal service, so any web page that can
// reach it cross-origin would effectively expose "this machine's decision capability" to the public internet. The platform
// already has the explicit --cors-origin switch; putting forwarding here leaves a single, controllable point of exposure.
//
// The cost is one extra hop for trial runs. A trial run is something a human clicks in the UI, not part of the production decision path, so that hop is acceptable.

const engineTimeout = 5 * time.Second

// GET /api/v1/engine —— engine self-description: is it alive, and which packages are loaded.
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
		// Alive but failing: report "alive, but the package list is unavailable" honestly instead of faking healthy=true
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

// POST /api/v1/trial —— forward verbatim to the engine's /v1/trial.
//
// The response (including errors) is passed through word for word: trial-run diagnostics belong to the engine, and the platform
// must not reinterpret them — once a reason code is rewritten here, the UI no longer shows what the engine actually said.
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
		// Forwarding a truncated body is worse than rejecting it: the engine would receive half a JSON document and report a parse error unrelated to the real cause.
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
