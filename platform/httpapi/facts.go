package httpapi

import (
	"net/http"

	"goldenrules/platform/contracts"
	"goldenrules/platform/facts"
)

// GET /api/v1/facts
func (s *Server) listFacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list := s.deps.Facts.List(q.Get("namespace"), q.Get("q"), q.Get("includeDeprecated") == "true")
	writeJSON(w, http.StatusOK, map[string]any{
		"catalogVersion": s.deps.Facts.Catalog().CatalogVersion,
		"total":          len(list),
		"items":          list,
	})
}

// POST /api/v1/facts
func (s *Server) createFact(w http.ResponseWriter, r *http.Request) {
	var in contracts.FactSchema
	if err := decode(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	created, issues, err := s.deps.Facts.Create(actor(r), in)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"fact": created, "issues": orEmpty(issues)})
}

// GET /api/v1/facts/{name}
func (s *Server) getFact(w http.ResponseWriter, r *http.Request) {
	f, ok := s.deps.Facts.Get(r.PathValue("name"))
	if !ok {
		fail(w, r, &facts.NotFoundError{Fact: r.PathValue("name")})
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// PUT /api/v1/facts/{name}
func (s *Server) updateFact(w http.ResponseWriter, r *http.Request) {
	var in contracts.FactSchema
	if err := decode(r, &in); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	updated, issues, err := s.deps.Facts.Update(actor(r), r.PathValue("name"), in)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fact": updated, "issues": orEmpty(issues)})
}

// DELETE /api/v1/facts/{name}
func (s *Server) deleteFact(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Facts.Delete(actor(r), r.PathValue("name")); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name")})
}

// POST /api/v1/facts/{name}/deprecate  {"replacedBy":"x.y"}
func (s *Server) deprecateFact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ReplacedBy string `json:"replacedBy"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	f, issues, err := s.deps.Facts.Deprecate(actor(r), r.PathValue("name"), body.ReplacedBy)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fact": f, "issues": orEmpty(issues)})
}

// GET /api/v1/facts/artifacts —— 只读预览生成物摘要（不落盘）
func (s *Server) getFactArtifacts(w http.ResponseWriter, r *http.Request) {
	arts, err := s.deps.Facts.Artifacts()
	if err != nil {
		fail(w, r, err)
		return
	}
	files := make([]map[string]any, 0, len(arts.Files))
	for _, f := range arts.Files {
		files = append(files, map[string]any{"name": f.Name, "digest": f.Digest, "bytes": len(f.Body)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"catalogVersion": arts.CatalogVersion,
		"files":          files,
		"slotCount":      len(arts.SlotTable),
	})
}

// POST /api/v1/facts/artifacts —— 幂等生成（内容相同不落盘）
func (s *Server) writeFactArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.ArtifactsDir == "" {
		writeError(w, r, http.StatusServiceUnavailable, "ARTIFACTS_DIR_UNSET", "未配置产物目录", nil)
		return
	}
	changed, err := s.deps.Facts.Write(s.deps.ArtifactsDir)
	if err != nil {
		fail(w, r, err)
		return
	}
	if changed == nil {
		changed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": s.deps.ArtifactsDir, "changed": changed})
}

// GET /api/v1/facts/artifacts/verify —— CI 门禁：生成物是否过期
func (s *Server) verifyFactArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.ArtifactsDir == "" {
		writeError(w, r, http.StatusServiceUnavailable, "ARTIFACTS_DIR_UNSET", "未配置产物目录", nil)
		return
	}
	drifts, err := s.deps.Facts.Verify(s.deps.ArtifactsDir)
	if err != nil {
		fail(w, r, err)
		return
	}
	status := http.StatusOK
	if len(drifts) > 0 {
		status = http.StatusConflict
	}
	if drifts == nil {
		drifts = []facts.Drift{}
	}
	writeJSON(w, status, map[string]any{"upToDate": len(drifts) == 0, "drifts": drifts})
}
