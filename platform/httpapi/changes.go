package httpapi

import (
	"net/http"
	"time"

	"goldenrules/platform/gov"
	"goldenrules/platform/packages"
)

// actorType 从 `X-Actor-Type` 取提交者/操作者类型；认不出时按 agent（最小权限，`gov.NormalizeActorType`）。
func actorType(r *http.Request) string {
	return gov.NormalizeActorType(r.Header.Get("X-Actor-Type"))
}

// GET /api/v1/approval-matrix —— 审批矩阵（前端渲染审批工作台 / 审计对账用）
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

// POST /api/v1/changes —— 提交变更单
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

// POST /api/v1/rollback  {packageId, toVersion, reason} —— `PLAN.md` §9 的扁平回滚入口
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

// GET /api/v1/audit?changeId=&packageId= —— 治理动作留痕（`contracts/04` §9）
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	evs := s.deps.Gov.Audits(q.Get("changeId"), q.Get("packageId"))
	if evs == nil {
		evs = []gov.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(evs), "items": evs})
}

// changeSummary 列表视图：去掉审批位明细以外的冗长字段。
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
		// 审批时效（contracts/04 §8）：列表页要能直接按「已超时」排序催办
		"sla": c.SLA(time.Now().UTC()),
	}
	if c.Emergency {
		out["emergency"] = true
		out["makeupReviewDueAt"] = c.MakeupReviewDueAt
		out["makeupReviewPending"] = c.MakeupReviewMissing()
	}
	return out
}

// POST /api/v1/changes/{id}/emergency-publish —— 紧急通道（contracts/04 §7）
//
// 紧急发布是例外路径，因此接口上把「共同授权人」单独列为字段而不是塞进 header：
// 双人授权必须能在请求体里被审计系统读到。
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

// POST /api/v1/changes/{id}/makeup-review —— 紧急发布后补审（contracts/04 §7）
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

// GET /api/v1/changes/pending-makeup-review —— 超时巡检：紧急发布但未补审的单子
func (s *Server) pendingMakeupReview(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Gov.PendingMakeupReviews()
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, changeSummary(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "items": out})
}

// publishGuard 发布前的治理检查。`Gov` 未接线（单包测试/演练）时放行，
// 但 `platformd` 一定接线——生产路径上「没有变更单就发布」是不存在的。
func (s *Server) publishGuard(r *http.Request, changeID string, pkg packages.Version) error {
	if s.deps.Gov == nil {
		return nil
	}
	_, err := s.deps.Gov.PermitPublish(changeID, pkg.PackageID, pkg.PackageVersion, actor(r), actorType(r))
	return err
}
