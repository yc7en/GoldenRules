package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goldenrules/platform/dsl"
	"goldenrules/platform/facts"
	"goldenrules/platform/gov"
	"goldenrules/platform/obs"
	"goldenrules/platform/packages"
	"goldenrules/platform/persist"
)

// newTestServer is for test cases that only care about HTTP behavior; cases that need to manipulate domain services directly use newTestEnv.
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newTestEnv(t)
	return h
}

// newTestEnv returns the handler together with its dependencies, so test cases can set up precondition state without going through HTTP.
func newTestEnv(t *testing.T) (http.Handler, Deps) {
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

	rsStore, err := persist.Open(filepath.Join(dir, "rulesets.json"), func() dsl.Repo { return dsl.Repo{} })
	if err != nil {
		t.Fatalf("open rulesets: %v", err)
	}
	rs := dsl.NewStore(rsStore)
	svc.WithRefChecker(rs.ReferencingFact)

	pkStore, err := persist.Open(filepath.Join(dir, "packages.json"), func() packages.Repo { return packages.Repo{} })
	if err != nil {
		t.Fatalf("open packages: %v", err)
	}
	pk := packages.NewService(pkStore).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})

	deps := Deps{Facts: svc, Rulesets: rs, Packages: pk, ArtifactsDir: filepath.Join(dir, "artifacts")}
	return New(deps), deps
}

// newGovTestServer adds a governance service on top of the same storage: approval/publish go through change requests,
// which is used to test the B9 closed loop; B8's bare actions are still covered by newTestServer (Gov is nil).
func newGovTestServer(t *testing.T) http.Handler {
	t.Helper()
	_, deps := newTestEnv(t)
	govStore, err := persist.Open(filepath.Join(t.TempDir(), "gov.json"), func() gov.Doc {
		return gov.Doc{Grays: map[string]gov.Gray{}, Seq: map[string]int{}}
	})
	if err != nil {
		t.Fatalf("open gov: %v", err)
	}
	deps.Gov = gov.NewService(govStore, deps.Packages).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})
	return New(deps)
}

// newObsTestServer adds a decision log service on top; withReplay=false means there is no replay backend.
// The key is hardcoded in the test (32-byte hex) and only serves to assert the encryption/decryption path locally.
func newObsTestServer(t *testing.T, withKey, withReplay bool) http.Handler {
	t.Helper()
	var rep obs.Replayer
	if withReplay {
		rep = stubReplayer{}
	}
	return newObsEnv(t, withKey, rep)
}

// newObsEnv assembles the environment with the given replay backend.
//
// It is factored out so that a **typed nil** (`(*obs.HTTPReplayer)(nil)`) can be injected — exactly the value
// real assembly produces when --engine-url is unset, and the value that once turned the log list into a 500.
func newObsEnv(t *testing.T, withKey bool, rep obs.Replayer) http.Handler {
	t.Helper()
	_, deps := newTestEnv(t)
	dir := t.TempDir()
	logStore, err := persist.Open(filepath.Join(dir, "logs.json"), func() obs.LogDoc {
		return obs.LogDoc{Seq: map[string]int{}}
	})
	if err != nil {
		t.Fatalf("open logs: %v", err)
	}
	rawStore, err := persist.Open(filepath.Join(dir, "raw.json"), func() obs.RawDoc {
		return obs.RawDoc{Blobs: map[string]obs.RawSnapshotRef{}}
	})
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	var box *obs.Box
	if withKey {
		box, err = obs.NewBox("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
		if err != nil {
			t.Fatalf("box: %v", err)
		}
	}
	deps.Obs = obs.New(logStore, rawStore, box).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})
	// Replay/log access must write an audit record, so the observation environment carries the governance service too.
	govStore, err := persist.Open(filepath.Join(t.TempDir(), "gov.json"), func() gov.Doc {
		return gov.Doc{Grays: map[string]gov.Gray{}, Seq: map[string]int{}}
	})
	if err != nil {
		t.Fatalf("open gov: %v", err)
	}
	deps.Gov = gov.NewService(govStore, deps.Packages).WithClock(func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	})
	if rep != nil {
		deps.Replayer = rep
	}
	return New(deps)
}

// doAs sends a request carrying the actor identity: `X-Actor` + `X-Actor-Type` (governance-side segregation of duties is decided from these).
func doAs(t *testing.T, h http.Handler, method, path, body, actor, actorType string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Actor", actor)
	req.Header.Set("X-Actor-Type", actorType)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// doRaw returns the raw response (the artifact is a bare JSON document, not a wrapper object).
func doRaw(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set("X-Actor", "engineer.A")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
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

	// Change the catalog without regenerating → the gate must report staleness
	if rec, _ := do(t, h, "POST", "/api/v1/facts",
		`{"fact":"sanctions.hit","type":"boolean","required":true,"version":"v1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create sanctions = %d %s", rec.Code, rec.Body)
	}
	rec, body = do(t, h, "GET", "/api/v1/facts/artifacts/verify", "")
	if rec.Code != http.StatusConflict || body["upToDate"] != false {
		t.Fatalf("过期生成物应返回 409，实际 %d %s", rec.Code, rec.Body)
	}
}
