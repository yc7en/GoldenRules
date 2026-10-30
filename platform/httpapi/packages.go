package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"goldenrules/platform/dsl"
	"goldenrules/platform/gov"
	"goldenrules/platform/packages"
)

// POST /api/v1/packages —— 组装 + 编译一个草稿版本（不发布）。
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

// GET /api/v1/packages?packageId= —— 列版本（不含包体，避免响应过大）。
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
// 治理接线后（`Deps.Gov != nil`）此接口**关闭**：审批是变更单的动作（`contracts/04` §3），
// 直接批版本等于绕过审批矩阵。保留分支只为单包测试与演练场景。
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
	// `contracts/01` §6.1：pending_review 的用例不阻断发布，但发布时必须显式提示。
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  summarize(v),
		"warnings": publishWarnings(v),
	})
}

// publishWarnings 发布时对前端/调用方的显式提示集合。
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

// GET /api/v1/packages/{pkg}/versions/{version}/artifact —— 交付给决策服务的规则包 JSON
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

// GET /api/v1/packages/{pkg}/versions/{version}/verify —— 验签（防篡改）
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

// summarize 列表/摘要视图：去掉包体（IR 可能很大），保留生命周期字段。
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
