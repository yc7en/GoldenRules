package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"goldenrules/platform/dsl"
	"goldenrules/platform/gov"
	"goldenrules/platform/packages"
)

// POST /api/v1/packages —— assemble + compile one draft version (no publish).
func (s *Server) createPackage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PackageID  string   `json:"packageId"`
		Version    string   `json:"version"`
		RulesetIDs []string `json:"rulesetIds"`
		RiskLevel  string   `json:"riskLevel"`
		Changelog  string   `json:"changelog"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	if len(req.RulesetIDs) == 0 {
		writeError(w, r, http.StatusBadRequest, packages.CodeEmptyPackage, "至少指定一个规则集", nil)
		return
	}

	defs := make([]dsl.Definition, 0, len(req.RulesetIDs))
	for _, id := range req.RulesetIDs {
		def, ok := s.deps.Rulesets.Get(id)
		if !ok {
			fail(w, r, &dsl.NotExistError{What: "规则集", ID: id})
			return
		}
		defs = append(defs, def)
	}

	v, reports, err := s.deps.Packages.CreateDraft(packages.BuildInput{
		PackageID: req.PackageID,
		Version:   req.Version,
		Rulesets:  defs,
		Catalog:   s.deps.Facts.Snapshot(),
		Actor:     actor(r),
		Changelog: req.Changelog,
		RiskLevel: req.RiskLevel,
	})
	if err != nil {
		var se *packages.StateError
		if errors.As(err, &se) && se.Code == packages.CodeCompileFailed {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"errorCode": se.Code,
				"message":   se.Message,
				"reports":   reports,
			})
			return
		}
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"version": summarize(v), "reports": reports})
}

// GET /api/v1/packages?packageId= —— list versions (without the package body, to keep the response small).
func (s *Server) listPackages(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Packages.List(r.URL.Query().Get("packageId"))
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		out = append(out, summarize(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(out), "items": out})
}

// GET /api/v1/packages/{pkg}/versions/{version}
func (s *Server) getPackageVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := s.deps.Packages.Get(r.PathValue("pkg"), r.PathValue("version"))
	if !ok {
		fail(w, r, &packages.NotFoundError{PackageID: r.PathValue("pkg"), Version: r.PathValue("version")})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// POST /api/v1/packages/{pkg}/versions/{version}/approve
//
// Once governance is wired in (`Deps.Gov != nil`) this endpoint is **closed**: approval is an action of the change request (`contracts/04` §3),
// and approving a version directly amounts to bypassing the approval matrix. The branch is kept only for single-package test and drill scenarios.
func (s *Server) approvePackage(w http.ResponseWriter, r *http.Request) {
	if s.deps.Gov != nil {
		writeError(w, r, http.StatusConflict, gov.CodeChangeRequired,
			"审批必须走变更单：POST /api/v1/changes 提交、POST /api/v1/changes/{id}/approve 审批", nil)
		return
	}
	v, err := s.deps.Packages.Approve(r.PathValue("pkg"), r.PathValue("version"), actor(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summarize(v))
}

// POST /api/v1/packages/{pkg}/versions/{version}/publish  {validFrom, validUntil}
func (s *Server) publishPackage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
		ChangeID   string `json:"changeId"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
			return
		}
	}
	target, ok := s.deps.Packages.Get(r.PathValue("pkg"), r.PathValue("version"))
	if !ok {
		fail(w, r, &packages.NotFoundError{PackageID: r.PathValue("pkg"), Version: r.PathValue("version")})
		return
	}
	if err := s.publishGuard(r, req.ChangeID, target); err != nil {
		fail(w, r, err)
		return
	}
	prev, _ := s.deps.Packages.Active(target.PackageID)

	v, err := s.deps.Packages.Publish(r.PathValue("pkg"), r.PathValue("version"), actor(r), req.ValidFrom, req.ValidUntil)
	if err != nil {
		fail(w, r, err)
		return
	}
	if s.deps.Gov != nil {
		s.deps.Gov.MarkPublished(req.ChangeID, v, actor(r), actorType(r), prev.PackageVersion, req.ValidFrom, req.ValidUntil)
	}
	// `contracts/01` §6.1: pending_review test cases do not block publish, but publish must warn about them explicitly.
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  summarize(v),
		"warnings": publishWarnings(v),
	})
}

// publishWarnings is the set of explicit warnings shown to the frontend/caller on publish.
func publishWarnings(v packages.Version) []string {
	out := []string{}
	if v.UnconfirmedTC > 0 {
		out = append(out, fmt.Sprintf("存在 %d 条未确认用例（approved 后才作为发布门禁）", v.UnconfirmedTC))
	}
	if v.ValidFrom != "" {
		out = append(out, "已设置 validFrom，包处于 published 但到点才生效")
	}
	return out
}

// POST /api/v1/packages/{pkg}/rollback  {toVersion, reason}
func (s *Server) rollbackPackage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToVersion string `json:"toVersion"`
		Reason    string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	if s.deps.Gov != nil {
		v, err := s.deps.Gov.Rollback(r.PathValue("pkg"), req.ToVersion, actor(r), actorType(r), req.Reason)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, summarize(v))
		return
	}
	v, err := s.deps.Packages.Rollback(r.PathValue("pkg"), req.ToVersion, actor(r), req.Reason)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summarize(v))
}

// GET /api/v1/packages/{pkg}/active
func (s *Server) activePackage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.deps.Packages.Active(r.PathValue("pkg"))
	if !ok {
		fail(w, r, &packages.NotFoundError{PackageID: r.PathValue("pkg")})
		return
	}
	writeJSON(w, http.StatusOK, summarize(v))
}

// GET /api/v1/packages/{pkg}/versions/{version}/artifact —— the rule package JSON handed to the decision service
func (s *Server) packageArtifact(w http.ResponseWriter, r *http.Request) {
	v, ok := s.deps.Packages.Get(r.PathValue("pkg"), r.PathValue("version"))
	if !ok {
		fail(w, r, &packages.NotFoundError{PackageID: r.PathValue("pkg"), Version: r.PathValue("version")})
		return
	}
	body, err := v.Artifact()
	if err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Package-Signature", v.Signature)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// GET /api/v1/packages/{pkg}/versions/{version}/verify —— signature verification (tamper detection)
func (s *Server) verifyPackage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.deps.Packages.Get(r.PathValue("pkg"), r.PathValue("version"))
	if !ok {
		fail(w, r, &packages.NotFoundError{PackageID: r.PathValue("pkg"), Version: r.PathValue("version")})
		return
	}
	ok2 := packages.Verify(v)
	status := http.StatusOK
	if !ok2 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{
		"valid":     ok2,
		"signature": v.Signature,
		"errorCode": map[bool]string{true: "", false: packages.CodeSignatureBad}[ok2],
	})
}

// GET /api/v1/packages/{pkg}/versions/{version}/testcases —— rehearsal of the publish gate.
//
// Read-only: runs (or evaluates per configuration) the approved test cases in the package and lays the results out. The actual blocking happens on publish.
// The value of a separate "rehearsal" entry point is that the releaser sees which test case is red before pressing publish,
// instead of receiving a bare "it was blocked" from the publish action.
func (s *Server) packageTestcases(w http.ResponseWriter, r *http.Request) {
	pkgID, version := r.PathValue("pkg"), r.PathValue("version")
	rep, err := s.deps.Packages.Verify(pkgID, version)
	if err != nil {
		fail(w, r, err)
		return
	}
	if rep.Failures == nil {
		rep.Failures = []packages.VerifyFailure{}
	}
	v, _ := s.deps.Packages.Get(pkgID, version)
	writeJSON(w, http.StatusOK, map[string]any{
		"report": rep,
		"gate": map[string]any{
			"enforcedOnPublish": true,
			"approvedCases":     v.ConfirmedTC,
			"unconfirmedCases":  v.UnconfirmedTC,
			"note":              "任一 approved 用例失败将阻断发布（contracts/01 §6.1）",
		},
	})
}

// summarize builds the list/summary view: drops the package body (the IR can be large) but keeps the lifecycle fields.
func summarize(v packages.Version) map[string]any {
	return map[string]any{
		"packageId":            v.PackageID,
		"packageVersion":       v.PackageVersion,
		"status":               v.Status,
		"factSchemaVersion":    v.FactSchemaVersion,
		"rulesetIds":           v.RulesetIDs,
		"ruleCount":            v.RuleCount,
		"unconfirmedTestCases": v.UnconfirmedTC,
		"confirmedTestCases":   v.ConfirmedTC,
		"riskLevel":            v.RiskLevel,
		"signature":            v.Signature,
		"validFrom":            v.ValidFrom,
		"validUntil":           v.ValidUntil,
		"publishedAt":          v.PublishedAt,
		"publishedBy":          v.PublishedBy,
		"retiredAt":            v.RetiredAt,
		"retiredCause":         v.RetiredCause,
		"supersededBy":         v.SupersededBy,
		"createdAt":            v.CreatedAt,
		"createdBy":            v.CreatedBy,
	}
}
