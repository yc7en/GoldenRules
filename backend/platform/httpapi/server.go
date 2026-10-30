// Package httpapi is the HTTP entry point of the BRMS management platform.
//
// It does exactly three things: routing, decoding, and translating domain errors into the ErrorContract of `contracts/05` §3.2.
// All business semantics live in the domain packages (facts / dsl / packages / gov / obs); this package never makes rule decisions.
//
// The API shape follows `PLAN.md` §9 "frontend integration interfaces": the frontend depends only on this layer.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
	"goldenrules/platform/facts"
	"goldenrules/platform/gov"
	"goldenrules/platform/obs"
	"goldenrules/platform/packages"
)

// Deps is the set of domain services the platform depends on. Filled in package by package since B6.
type Deps struct {
	Facts    *facts.Service
	Rulesets *dsl.Store
	Packages *packages.Service
	// A nil Gov degrades to a "governance-free" mode: approval/publish become bare actions, intended only for single-package tests and drills.
	// platformd always wires it in — the production path has no "publish directly, skipping the change request".
	Gov *gov.Service
	// Obs is the decision log; a nil Replayer means replay is unavailable (no decision service address configured).
	Obs          *obs.Service
	Replayer     obs.Replayer
	ArtifactsDir string
	// A non-empty CORSOrigin adds a cross-origin header to responses, so a local integration console can call the API directly from file://.
	// Empty by default (disabled): the platform has no authentication, so making "anyone may read cross-origin" the default posture would be wrong.
	CORSOrigin string
	// EngineURL/EngineToken are the decision service address. Only when set do we expose "engine self-description" and "trial-run forwarding",
	// so the browser never has to reach the engine directly (the engine does not enable CORS; it is an unauthenticated internal service).
	EngineURL   string
	EngineToken string
}

// Server holds the dependencies and the routing table.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// New builds the HTTP handler. It returns http.Handler rather than *Server so callers cannot reach into the internals.
func New(d Deps) http.Handler {
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.routes()
	if d.CORSOrigin == "" {
		return s.mux
	}
	return withCORS(s.mux, d.CORSOrigin)
}

// withCORS allows the given origin on every response.
//
// Two constraints:
//   - Only the self-reported actor headers (X-Actor/X-Actor-Type) are allowed; arbitrary custom headers are not opened up —
//     which headers a cross-origin page may send determines who that page can impersonate.
//   - Preflight returns 204 directly, keeping OPTIONS out of the routing table (Go 1.22's mux answers 405 when OPTIONS is unregistered,
//     and browsers treat 405 as a cross-origin failure, whose symptom is "clicking the button does nothing").
func withCORS(next http.Handler, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-Actor, X-Actor-Type, X-Reload-Token")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	// Health check: reports only that the process is alive; it does not probe the decision service (the platform does not depend on the decision service at runtime, A-1).
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"service":    "goldenrules-platform",
			"timeLayout": contracts.GoldenTimeLayout,
		})
	})

	// ---- B6 fact catalog ----
	s.mux.HandleFunc("GET /api/v1/facts", s.listFacts)
	s.mux.HandleFunc("POST /api/v1/facts", s.createFact)
	s.mux.HandleFunc("GET /api/v1/facts/artifacts", s.getFactArtifacts)
	s.mux.HandleFunc("POST /api/v1/facts/artifacts", s.writeFactArtifacts)
	s.mux.HandleFunc("GET /api/v1/facts/artifacts/verify", s.verifyFactArtifacts)
	s.mux.HandleFunc("GET /api/v1/facts/{name}", s.getFact)
	s.mux.HandleFunc("PUT /api/v1/facts/{name}", s.updateFact)
	s.mux.HandleFunc("DELETE /api/v1/facts/{name}", s.deleteFact)
	s.mux.HandleFunc("POST /api/v1/facts/{name}/deprecate", s.deprecateFact)

	// ---- B7 ruleset editing and dry-run ----
	s.mux.HandleFunc("GET /api/v1/rulesets", s.listRulesets)
	s.mux.HandleFunc("GET /api/v1/rulesets/{id}", s.getRuleset)
	s.mux.HandleFunc("PUT /api/v1/rulesets/{id}", s.putRuleset)
	s.mux.HandleFunc("DELETE /api/v1/rulesets/{id}", s.deleteRuleset)
	s.mux.HandleFunc("POST /api/v1/rulesets/{id}/validate", s.validateRuleset)

	// ---- B8 compilation and rule package publish ----
	s.mux.HandleFunc("GET /api/v1/packages", s.listPackages)
	s.mux.HandleFunc("POST /api/v1/packages", s.createPackage)
	s.mux.HandleFunc("GET /api/v1/packages/{pkg}/active", s.activePackage)
	s.mux.HandleFunc("POST /api/v1/packages/{pkg}/rollback", s.rollbackPackage)
	s.mux.HandleFunc("GET /api/v1/packages/{pkg}/versions/{version}", s.getPackageVersion)
	s.mux.HandleFunc("GET /api/v1/packages/{pkg}/versions/{version}/artifact", s.packageArtifact)
	s.mux.HandleFunc("GET /api/v1/packages/{pkg}/versions/{version}/verify", s.verifyPackage)
	// F-PKG06: gate rehearsal for approved test cases (enforced automatically on publish)
	s.mux.HandleFunc("GET /api/v1/packages/{pkg}/versions/{version}/testcases", s.packageTestcases)
	s.mux.HandleFunc("POST /api/v1/packages/{pkg}/versions/{version}/approve", s.approvePackage)
	s.mux.HandleFunc("POST /api/v1/packages/{pkg}/versions/{version}/publish", s.publishPackage)

	// ---- B9 approval matrix and publish governance ----
	s.mux.HandleFunc("GET /api/v1/approval-matrix", s.approvalMatrix)
	s.mux.HandleFunc("GET /api/v1/changes", s.listChanges)
	s.mux.HandleFunc("POST /api/v1/changes", s.createChange)
	s.mux.HandleFunc("GET /api/v1/changes/{id}", s.getChange)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/approve", s.approveChange)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/reject", s.rejectChange)
	// Emergency channel (contracts/04 §7): emergency publish, post-hoc makeup review, patrol for pending makeup reviews
	s.mux.HandleFunc("POST /api/v1/changes/{id}/emergency-publish", s.emergencyPublish)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/makeup-review", s.makeupReview)
	s.mux.HandleFunc("GET /api/v1/changes/pending-makeup-review", s.pendingMakeupReview)
	s.mux.HandleFunc("GET /api/v1/packages/{pkg}/gray", s.getGray)
	s.mux.HandleFunc("PUT /api/v1/packages/{pkg}/gray", s.putGray)
	s.mux.HandleFunc("POST /api/v1/rollback", s.rollbackFlat)
	s.mux.HandleFunc("GET /api/v1/audit", s.listAudit)

	// ---- B10 decision log and audit ----
	s.mux.HandleFunc("POST /api/v1/logs", s.ingestLogs)
	s.mux.HandleFunc("GET /api/v1/logs", s.listLogs)
	s.mux.HandleFunc("GET /api/v1/logs/stats", s.logStats)
	// Key self-description and rotation (`contracts/03` §8.2). The literal key segment takes precedence over {id}, so it does not collide with the detail route.
	s.mux.HandleFunc("GET /api/v1/logs/key", s.getLogKey)
	s.mux.HandleFunc("POST /api/v1/logs/key/rotate", s.rotateLogKey)
	s.mux.HandleFunc("POST /api/v1/logs/key/rewrap", s.rewrapLogKey)
	s.mux.HandleFunc("GET /api/v1/logs/{id}", s.getLog)
	s.mux.HandleFunc("POST /api/v1/logs/{id}/replay", s.replayLog)

	// ---- decision service forwarding (the UI never talks to the engine directly) ----
	s.mux.HandleFunc("GET /api/v1/engine", s.engineInfo)
	s.mux.HandleFunc("POST /api/v1/trial", s.engineTrial)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

// writeError emits real errors (service-level/request-level). Note that contract violations do not belong here:
// a contract violation on the decision side becomes a REJECT decision and does not go through ErrorContract (`contracts/05` §1).
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]json.RawMessage) {
	writeJSON(w, status, contracts.ErrorContract{
		ErrorCode: code,
		Message:   msg,
		TraceID:   r.Header.Get("X-Trace-Id"),
		Details:   details,
	})
}

// fail translates domain errors into HTTP. The mapping lives in one place so each handler does not write its own.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var vErr *facts.ValidationError
	var nErr *facts.NotFoundError
	var iErr *facts.InUseError
	var neErr *dsl.NotExistError
	var pkErr *packages.NotFoundError
	var stErr *packages.StateError
	var gErr *gov.Error
	var gnErr *gov.NotFoundError
	var oErr *obs.Error

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
	case errors.As(err, &pkErr):
		writeError(w, r, http.StatusNotFound, packages.CodeNotFound, pkErr.Error(), nil)
	case errors.As(err, &stErr):
		status := http.StatusConflict
		// "Approved test cases exist but nobody can run them" is not a state conflict but a missing platform capability → 503:
		// the caller should fix the configuration, not the package.
		if stErr.Code == packages.CodeVerifyUnavailable {
			status = http.StatusServiceUnavailable
		}
		writeError(w, r, status, stErr.Code, stErr.Message, nil)
	case errors.As(err, &iErr):
		body, _ := json.Marshal(iErr.Referers)
		writeError(w, r, http.StatusConflict, facts.CodeInUse, iErr.Error(), map[string]json.RawMessage{
			"referers": body,
		})
	case errors.As(err, &gnErr):
		writeError(w, r, http.StatusNotFound, gov.CodeChangeNotFound, gnErr.Error(), nil)
	case errors.As(err, &gErr):
		writeError(w, r, govStatus(gErr.Code), gErr.Code, gErr.Message, nil)
	case errors.As(err, &oErr):
		writeError(w, r, obsStatus(oErr.Code), oErr.Code, oErr.Message, nil)
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
	}
}

// govStatus maps governance reason codes → HTTP status.
//
// Three categories: **declarative refusal** (matrix does not allow, segregation of duties, agent overreach) is 403;
// **wrong state** (already approved, published without approval, version mismatch) is 409; **bad argument** (unknown level/canary value) is 400/422.
func govStatus(code string) int {
	switch code {
	case gov.CodeMatrixRejected, gov.CodeSelfApproval, gov.CodeAgentCannotApprove,
		gov.CodeAgentCannotPublish, gov.CodeSoDApprovePublish, gov.CodeSoDSubmitPublish:
		return http.StatusForbidden
	case gov.CodeInvalidArgument:
		return http.StatusBadRequest
	case gov.CodeGrayInvalid:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusConflict
	}
}

// obsStatus maps observation reason codes → HTTP status.
// "Not found" is 404; "replay not possible yet" is 501 (the service is not broken, the capability is unconfigured); the rest are request errors, 4xx.
func obsStatus(code string) int {
	switch code {
	case obs.CodeLogNotFound:
		return http.StatusNotFound
	case obs.CodeInvalidLog, obs.CodeKeyMissing, obs.CodeRawUnavailable, obs.CodeRawPurged:
		return http.StatusUnprocessableEntity
	case obs.CodeKeyInvalid:
		return http.StatusBadRequest
	case obs.CodeKeyUnknown:
		// Not holding the key is not a "bad argument": the request is perfectly valid, this key simply is not configured here → 409,
		// so the caller provisions the key instead of changing the request.
		return http.StatusConflict
	case obs.CodeReplayUnavailable:
		return http.StatusNotImplemented
	case obs.CodeDuplicateLog:
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

// decode reads the JSON body and rejects unknown fields (the contract is closed; an extra field usually signals a typo).
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

// orEmpty normalizes nil diagnostics into an empty slice: in JSON, `null` and `[]` are two different branches for the frontend, so both become the latter.
func orEmpty(issues []facts.Issue) []facts.Issue {
	if issues == nil {
		return []facts.Issue{}
	}
	return issues
}
