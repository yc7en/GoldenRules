package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEngine 扮演决策服务：只有界面上用得到的三个端点。
func fakeEngine(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/v1/packages":
			_, _ = io.WriteString(w, `{"packages":[{"packageId":"pkg-x","packageVersion":"v1","rules":3,"rulesets":1}]}`)
		case "/v1/trial":
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if in["snapshot"] == nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"errorCode":"CONTRACT_INVALID","message":"缺 snapshot"}`)
				return
			}
			_, _ = io.WriteString(w, `{"decision":"APPROVE","reasonCode":"OK","packageId":"pkg-x","packageVersion":"v1","hitRules":[],"evaluatedRules":1,"outputs":{},"degraded":false,"degradedReasons":[],"executionTimeMs":0.01,"snapshotId":"s","rulesetId":"rs","rulesetVersion":"v1"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// 没配 --engine-url 时必须明说「没配置」，不能伪装成「连不上」。
func TestEngineInfoNotConfigured(t *testing.T) {
	rec, body := do2(New(Deps{}), "GET", "/api/v1/engine", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置应 503，实际 %d", rec.Code)
	}
	if body["errorCode"] != "ENGINE_NOT_CONFIGURED" {
		t.Fatalf("错误码应为 ENGINE_NOT_CONFIGURED: %v", body)
	}
}

func TestEngineInfoReportsPackages(t *testing.T) {
	eng := fakeEngine(t)
	defer eng.Close()
	rec, body := do2(New(Deps{EngineURL: eng.URL}), "GET", "/api/v1/engine", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("engine info = %d %s", rec.Code, rec.Body)
	}
	if body["healthy"] != true || body["baseUrl"] != eng.URL {
		t.Fatalf("应自述健康与地址: %v", body)
	}
	pkgs, ok := body["packages"].(map[string]any)
	if !ok {
		t.Fatalf("应原样透传包清单: %v", body["packages"])
	}
	items := pkgs["packages"].([]any)
	if len(items) != 1 {
		t.Fatalf("包清单条数不对: %v", items)
	}
}

// 引擎没起时是 502 + ENGINE_UNREACHABLE：这是「配置对了但对面没活」，与 503 不同。
func TestEngineInfoUnreachable(t *testing.T) {
	rec, body := do2(New(Deps{EngineURL: "http://127.0.0.1:1"}), "GET", "/api/v1/engine", "")
	if rec.Code != http.StatusBadGateway || body["errorCode"] != "ENGINE_UNREACHABLE" {
		t.Fatalf("应 502 ENGINE_UNREACHABLE，实际 %d %v", rec.Code, body)
	}
}

// 试跑：请求体与响应（含错误）都要逐字转发，平台不二次解释引擎的诊断。
func TestTrialProxiesBodyAndStatus(t *testing.T) {
	eng := fakeEngine(t)
	defer eng.Close()
	h := New(Deps{EngineURL: eng.URL})

	rec, body := do2(h, "POST", "/api/v1/trial", `{"snapshot":{"snapshotId":"s"},"context":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("试跑应透传 200，实际 %d %s", rec.Code, rec.Body)
	}
	if body["decision"] != "APPROVE" || body["packageId"] != "pkg-x" {
		t.Fatalf("响应应原样透传: %v", body)
	}

	// 引擎侧的错误码要原样回来，不能被平台改写成自己的错误
	rec, body = do2(h, "POST", "/api/v1/trial", `{"context":{}}`)
	if rec.Code != http.StatusBadRequest || body["errorCode"] != "CONTRACT_INVALID" {
		t.Fatalf("引擎 400 应原样透传，实际 %d %v", rec.Code, body)
	}
}

func TestTrialWithoutEngine(t *testing.T) {
	rec, body := do2(New(Deps{}), "POST", "/api/v1/trial", `{}`)
	if rec.Code != http.StatusServiceUnavailable || body["errorCode"] != "ENGINE_NOT_CONFIGURED" {
		t.Fatalf("未配置应 503，实际 %d %v", rec.Code, body)
	}
}

func TestTrialRejectsHugeBody(t *testing.T) {
	h := New(Deps{EngineURL: "http://127.0.0.1:1"})
	huge := `{"snapshot":{"x":"` + strings.Repeat("a", 4<<20) + `"}}`
	rec, body := do2(h, "POST", "/api/v1/trial", huge)
	// 413 而不是 400：请求体本身没写错，是太大了。截断转发会让引擎报一个假错误。
	if rec.Code != http.StatusRequestEntityTooLarge || body["errorCode"] != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("超过 4MB 应 413 PAYLOAD_TOO_LARGE，实际 %d %v", rec.Code, body)
	}
}

// do2 发一个请求并解析 JSON 响应（错误也解析，便于断言 errorCode）。
func do2(h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}
