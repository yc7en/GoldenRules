// Package httpapi 是 BRMS 管理平台的 HTTP 入口。
//
// 只做三件事：路由、解码、把领域错误翻译成 `contracts/05` §3.2 的 ErrorContract。
// 业务语义一律在领域包里（facts / dsl / packages / gov / obs），本包不写规则判断。
//
// API 形状对齐 `PLAN.md` §9「前端联调接口」：前端只依赖这一层。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
	"goldenrules/platform/facts"
)

// Deps 是平台依赖的领域服务集合。B6 起逐包填充。
type Deps struct {
	Facts        *facts.Service
	Rulesets     *dsl.Store
	ArtifactsDir string
}

// Server 持有依赖与路由表。
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// New 构造 HTTP handler。返回 http.Handler 而不是 *Server，避免调用方摸内部。
func New(d Deps) http.Handler {
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.routes()
	return s.mux
}

func (s *Server) routes() {
	// 健康检查：只报进程活着，不探决策服务（平台不依赖决策服务运行时，A-1）。
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"service":    "goldenrules-platform",
			"timeLayout": contracts.GoldenTimeLayout,
		})
	})

	// ---- B6 事实目录 ----
	s.mux.HandleFunc("GET /api/v1/facts", s.listFacts)
	s.mux.HandleFunc("POST /api/v1/facts", s.createFact)
	s.mux.HandleFunc("GET /api/v1/facts/artifacts", s.getFactArtifacts)
	s.mux.HandleFunc("POST /api/v1/facts/artifacts", s.writeFactArtifacts)
	s.mux.HandleFunc("GET /api/v1/facts/artifacts/verify", s.verifyFactArtifacts)
	s.mux.HandleFunc("GET /api/v1/facts/{name}", s.getFact)
	s.mux.HandleFunc("PUT /api/v1/facts/{name}", s.updateFact)
	s.mux.HandleFunc("DELETE /api/v1/facts/{name}", s.deleteFact)
	s.mux.HandleFunc("POST /api/v1/facts/{name}/deprecate", s.deprecateFact)

	// ---- B7 规则集编辑与 dry-run ----
	s.mux.HandleFunc("GET /api/v1/rulesets", s.listRulesets)
	s.mux.HandleFunc("GET /api/v1/rulesets/{id}", s.getRuleset)
	s.mux.HandleFunc("PUT /api/v1/rulesets/{id}", s.putRuleset)
	s.mux.HandleFunc("DELETE /api/v1/rulesets/{id}", s.deleteRuleset)
	s.mux.HandleFunc("POST /api/v1/rulesets/{id}/validate", s.validateRuleset)
}

// ---------------------------------------------------------------------------
// 通用工具
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// writeError 输出真错误（服务级/请求级）。注意契约违规不在这里：
// 决策侧的契约违规落 REJECT 决策，不走 ErrorContract（`contracts/05` §1）。
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]json.RawMessage) {
	writeJSON(w, status, contracts.ErrorContract{
		ErrorCode: code,
		Message:   msg,
		TraceID:   r.Header.Get("X-Trace-Id"),
		Details:   details,
	})
}

// fail 把领域错误翻译成 HTTP。映射集中在一处，避免每个 handler 各写一套。
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var vErr *facts.ValidationError
	var nErr *facts.NotFoundError
	var iErr *facts.InUseError
	var neErr *dsl.NotExistError

	switch {
	case errors.As(err, &vErr):
		code := "VALIDATION_FAILED"
		if len(vErr.Issues) > 0 {
			code = vErr.Issues[0].Code
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"errorCode": code,
			"message":   vErr.Error(),
			"traceId":   r.Header.Get("X-Trace-Id"),
			"issues":    vErr.Issues,
		})
	case errors.As(err, &nErr):
		writeError(w, r, http.StatusNotFound, facts.CodeNotFound, nErr.Error(), nil)
	case errors.As(err, &neErr):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", neErr.Error(), nil)
	case errors.As(err, &iErr):
		body, _ := json.Marshal(iErr.Referers)
		writeError(w, r, http.StatusConflict, facts.CodeInUse, iErr.Error(), map[string]json.RawMessage{
			"referers": body,
		})
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	}
}

// decode 读 JSON body，拒绝未知字段（契约是封闭的，多写字段通常是拼错的信号）。
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("请求体包含多余内容")
	}
	return nil
}

func actor(r *http.Request) string {
	if a := r.Header.Get("X-Actor"); a != "" {
		return a
	}
	return "anonymous"
}

// orEmpty 把 nil 诊断规范成空数组：JSON 里 `null` 与 `[]` 对前端是两种分支，统一成后者。
func orEmpty(issues []facts.Issue) []facts.Issue {
	if issues == nil {
		return []facts.Issue{}
	}
	return issues
}
