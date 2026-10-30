package httpapi

import (
	"net/http"
	"time"

	"goldenrules/platform/gov"
	"goldenrules/platform/packages"
)

// actorType reads the submitter/actor type from `X-Actor-Type`; when unrecognized it falls back to agent (least privilege, `gov.NormalizeActorType`).
func actorType(r *http.Request) string {
	return gov.NormalizeActorType(r.Header.Get("X-Actor-Type"))
}

// GET /api/v1/approval-matrix —— approval matrix (used by the frontend to render the approval workbench / reconcile audits)
func (s *Server) approvalMatrix(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"cells": gov.Describe()})
}

// GET /api/v1/changes?status=&packageId=
func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list := s.deps.Gov.List(q.Get("status"), q.Get("packageId"))
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, changeSummary(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "items": out})
}

// POST /api/v1/changes —— submit a change request
func (s *Server) createChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PackageID      string   `json:"packageId"`
		PackageVersion string   `json:"packageVersion"`
		ChangeType     string   `json:"changeType"`
		RiskLevel      string   `json:"riskLevel"`
		Reason         string   `json:"reason"`
		Scope          string   `json:"scope"`
		SubmitterType  string   `json:"submitterType"`
		Materials      []string `json:"materials"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	submitterType := req.SubmitterType
	if submitterType == "" {
		submitterType = actorType(r)
	}

	c, err := s.deps.Gov.Submit(gov.SubmitInput{
		PackageID:      req.PackageID,
		PackageVersion: req.PackageVersion,
		ChangeType:     req.ChangeType,
		RiskLevel:      req.RiskLevel,
		Reason:         req.Reason,
		Scope:          req.Scope,
		Submitter:      actor(r),
		SubmitterType:  submitterType,
		Materials:      req.Materials,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// GET /api/v1/changes/{id}
func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	c, ok := s.deps.Gov.Get(r.PathValue("id"))
	if !ok {
		fail(w, r, &gov.NotFoundError{ChangeID: r.PathValue("id")})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// POST /api/v1/changes/{id}/approve  {role, comment}
func (s *Server) approveChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role    string `json:"role"`
		Comment string `json:"comment"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	c, err := s.deps.Gov.Approve(r.PathValue("id"), actor(r), actorType(r), req.Role, req.Comment)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"change": c, "remaining": len(c.Remaining())})
}

// POST /api/v1/changes/{id}/reject  {role, comment}
func (s *Server) rejectChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role    string `json:"role"`
		Comment string `json:"comment"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	c, err := s.deps.Gov.Reject(r.PathValue("id"), actor(r), actorType(r), req.Role, req.Comment)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// GET /api/v1/packages/{pkg}/gray
func (s *Server) getGray(w http.ResponseWriter, r *http.Request) {
	g, ok := s.deps.Gov.Gray(r.PathValue("pkg"))
	if !ok {
		fail(w, r, &gov.NotFoundError{PackageID: r.PathValue("pkg")})
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// PUT /api/v1/packages/{pkg}/gray  {version, mode, percentage, grayTags, reason}
func (s *Server) putGray(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version    string   `json:"version"`
		Mode       string   `json:"mode"`
		Percentage int      `json:"percentage"`
		GrayTags   []string `json:"grayTags"`
		Reason     string   `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	g, warns, err := s.deps.Gov.SetGray(gov.GrayInput{
		PackageID:  r.PathValue("pkg"),
		Version:    req.Version,
		Mode:       req.Mode,
		Percentage: req.Percentage,
		GrayTags:   req.GrayTags,
		Reason:     req.Reason,
		Actor:      actor(r),
		ActorType:  actorType(r),
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gray": g, "warnings": warns})
}

// POST /api/v1/rollback  {packageId, toVersion, reason} —— the flat rollback entry point from `PLAN.md` §9
func (s *Server) rollbackFlat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PackageID string `json:"packageId"`
		ToVersion string `json:"toVersion"`
		Reason    string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	v, err := s.deps.Gov.Rollback(req.PackageID, req.ToVersion, actor(r), actorType(r), req.Reason)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summarize(v))
}

// GET /api/v1/audit?changeId=&packageId= —— audit trail of governance actions (`contracts/04` §9)
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	evs := s.deps.Gov.Audits(q.Get("changeId"), q.Get("packageId"))
	if evs == nil {
		evs = []gov.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(evs), "items": evs})
}

// changeSummary builds the list view: drops the long fields beyond the approval-slot details.
func changeSummary(c gov.Change) map[string]any {
	out := map[string]any{
		"changeId":       c.ChangeID,
		"packageId":      c.PackageID,
		"packageVersion": c.PackageVersion,
		"riskLevel":      c.RiskLevel,
		"changeType":     c.ChangeType,
		"status":         c.Status,
		"submitter":      c.Submitter,
		"submitterType":  c.SubmitterType,
		"required":       len(c.Slots),
		"approved":       len(c.Approvals),
		"remaining":      len(c.Remaining()),
		"createdAt":      c.CreatedAt,
		// Approval SLA (contracts/04 §8): the list page must be able to sort directly by "already overdue" to chase approvers
		"sla": c.SLA(time.Now().UTC()),
	}
	if c.Emergency {
		out["emergency"] = true
		out["makeupReviewDueAt"] = c.MakeupReviewDueAt
		out["makeupReviewPending"] = c.MakeupReviewMissing()
	}
	return out
}

// POST /api/v1/changes/{id}/emergency-publish —— emergency channel (contracts/04 §7)
//
// Emergency publish is an exception path, so the "co-authorizer" is an explicit request field rather than a header:
// two-person authorization must be readable by the audit system from the request body.
func (s *Server) emergencyPublish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason          string `json:"reason"`
		ValidFrom       string `json:"validFrom"`
		ValidUntil      string `json:"validUntil"`
		SecondActor     string `json:"secondActor"`
		SecondActorType string `json:"secondActorType"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	c, err := s.deps.Gov.EmergencyPublish(gov.EmergencyInput{
		ChangeID:        r.PathValue("id"),
		Actor:           actor(r),
		ActorType:       actorType(r),
		Reason:          req.Reason,
		ValidFrom:       req.ValidFrom,
		ValidUntil:      req.ValidUntil,
		SecondActor:     req.SecondActor,
		SecondActorType: req.SecondActorType,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"change":              c,
		"emergency":           true,
		"makeupReviewDueAt":   c.MakeupReviewDueAt,
		"pendingMakeupReview": c.MakeupReviewMissing(),
	})
}

// POST /api/v1/changes/{id}/makeup-review —— post-hoc review after an emergency publish (contracts/04 §7)
func (s *Server) makeupReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role    string `json:"role"`
		Comment string `json:"comment"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	c, err := s.deps.Gov.MakeupReview(r.PathValue("id"), actor(r), actorType(r), req.Role, req.Comment)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// GET /api/v1/changes/pending-makeup-review —— overdue patrol: change requests published under emergency without makeup review
func (s *Server) pendingMakeupReview(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Gov.PendingMakeupReviews()
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, changeSummary(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "items": out})
}

// publishGuard is the governance check before publish. It lets the call through when `Gov` is not wired in (single-package tests/drills),
// but `platformd` always wires it in — "publish without a change request" does not exist on the production path.
func (s *Server) publishGuard(r *http.Request, changeID string, pkg packages.Version) error {
	if s.deps.Gov == nil {
		return nil
	}
	_, err := s.deps.Gov.PermitPublish(changeID, pkg.PackageID, pkg.PackageVersion, actor(r), actorType(r))
	return err
}
