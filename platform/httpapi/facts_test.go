package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goldenrules/platform/facts"
	"goldenrules/platform/persist"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := persist.Open(filepath.Join(dir, "facts.json"), func() facts.Catalog {
		return facts.Catalog{CatalogVersion: "catalog-v2026.10.0"}
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := facts.New(st).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})
	return New(Deps{Facts: svc, ArtifactsDir: filepath.Join(dir, "artifacts")})
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Actor", "engineer.A")
	req.Header.Set("X-Trace-Id", "trace-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

const amountJSON = `{
  "fact": "request.amount",
  "displayName": "本次提款金额",
  "type": "decimal",
  "unit": "CNY",
  "semantics": "本次请求的提款本金",
  "required": true,
  "consistency": "STRONG",
  "sensitivity": "PII",
  "version": "v1"
}`

func TestFactCRUDOverHTTP(t *testing.T) {
	h := newTestServer(t)

	rec, body := do(t, h, "POST", "/api/v1/facts", amountJSON)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body)
	}
	if _, ok := body["fact"]; !ok {
		t.Fatalf("create 响应缺 fact: %s", rec.Body)
	}

	rec, body = do(t, h, "GET", "/api/v1/facts", "")
	if rec.Code != http.StatusOK || int(body["total"].(float64)) != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	rec, _ = do(t, h, "GET", "/api/v1/facts/request.amount", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}

	rec, body = do(t, h, "GET", "/api/v1/facts/request.nope", "")
	if rec.Code != http.StatusNotFound || body["errorCode"] != facts.CodeNotFound {
		t.Fatalf("get missing = %d %s", rec.Code, rec.Body)
	}

	rec, body = do(t, h, "PUT", "/api/v1/facts/request.amount",
		strings.Replace(amountJSON, `"version": "v1"`, `"version": "v2"`, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}

	rec, _ = do(t, h, "DELETE", "/api/v1/facts/request.amount", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	rec, _ = do(t, h, "GET", "/api/v1/facts/request.amount", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删除后应 404，实际 %d", rec.Code)
	}
}

func TestFactValidationReturnsIssues(t *testing.T) {
	h := newTestServer(t)
	bad := `{"fact":"request.amount","type":"decimal","required":true,"hasDefault":true,"default":"100","version":"v1"}`
	rec, body := do(t, h, "POST", "/api/v1/facts", bad)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("期望 422，实际 %d %s", rec.Code, rec.Body)
	}
	if body["errorCode"] != facts.CodeRequiredDefault {
		t.Fatalf("errorCode = %v", body["errorCode"])
	}
	issues, ok := body["issues"].([]any)
	if !ok || len(issues) == 0 {
		t.Fatalf("响应缺 issues: %s", rec.Body)
	}
	first := issues[0].(map[string]any)
	if first["path"] == "" || first["message"] == "" {
		t.Fatalf("诊断缺 path/message: %+v", first)
	}
}

func TestFactArtifactsAreIdempotent(t *testing.T) {
	h := newTestServer(t)
	if rec, _ := do(t, h, "POST", "/api/v1/facts", amountJSON); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}

	rec, body := do(t, h, "POST", "/api/v1/facts/artifacts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("write artifacts = %d %s", rec.Code, rec.Body)
	}
	if n := len(body["changed"].([]any)); n != 3 {
		t.Fatalf("首次应生成 3 个文件，实际 %d", n)
	}

	rec, body = do(t, h, "POST", "/api/v1/facts/artifacts", "")
	if n := len(body["changed"].([]any)); n != 0 {
		t.Fatalf("幂等失败，第二次改了 %d 个文件", n)
	}

	rec, body = do(t, h, "GET", "/api/v1/facts/artifacts/verify", "")
	if rec.Code != http.StatusOK || body["upToDate"] != true {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body)
	}

	// 改目录但不重新生成 → 门禁必须报过期
	if rec, _ := do(t, h, "POST", "/api/v1/facts",
		`{"fact":"sanctions.hit","type":"boolean","required":true,"version":"v1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create sanctions = %d %s", rec.Code, rec.Body)
	}
	rec, body = do(t, h, "GET", "/api/v1/facts/artifacts/verify", "")
	if rec.Code != http.StatusConflict || body["upToDate"] != false {
		t.Fatalf("过期生成物应返回 409，实际 %d %s", rec.Code, rec.Body)
	}
}
