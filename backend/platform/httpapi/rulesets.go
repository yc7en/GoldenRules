package httpapi

import (
	"net/http"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
)

// GET /api/v1/rulesets?scene=
func (s *Server) listRulesets(w http.ResponseWriter, r *http.Request) {
	items := s.deps.Rulesets.List(r.URL.Query().Get("scene"))
	if items == nil {
		items = []dsl.Definition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(items), "items": items})
}

// GET /api/v1/rulesets/{id}
func (s *Server) getRuleset(w http.ResponseWriter, r *http.Request) {
	def, ok := s.deps.Rulesets.Get(r.PathValue("id"))
	if !ok {
		fail(w, r, &dsl.NotExistError{What: "规则集", ID: r.PathValue("id")})
		return
	}
	writeJSON(w, http.StatusOK, def)
}

// PUT /api/v1/rulesets/{id} —— a save must pass dry-run first, otherwise 422 with the diagnostics.
func (s *Server) putRuleset(w http.ResponseWriter, r *http.Request) {
	var def dsl.Definition
	if err := decode(r, &def); err != nil {
		writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
		return
	}
	if def.RulesetID == "" {
		def.RulesetID = r.PathValue("id")
	}
	if def.RulesetID != r.PathValue("id") {
		writeError(w, r, http.StatusBadRequest, "RULESET_ID_MISMATCH", "URL 里的规则集 ID 与 body 不一致", nil)
		return
	}

	rep := s.analyze(def, nil)
	if !rep.Valid {
		writeJSON(w, http.StatusUnprocessableEntity, rep)
		return
	}
	saved, err := s.deps.Rulesets.Put(actor(r), def)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ruleset": saved, "report": rep})
}

// DELETE /api/v1/rulesets/{id}
func (s *Server) deleteRuleset(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Rulesets.Delete(r.PathValue("id")); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// POST /api/v1/rulesets/{id}/validate —— dry-run: writes nothing, publishes nothing.
//
// The body is optional (validate the saved version), or it may carry `definition` to validate unsaved edits;
// `outputSchema` can override the scene output schema that ships with the ruleset.
func (s *Server) validateRuleset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Definition   *dsl.Definition         `json:"definition"`
		OutputSchema *contracts.OutputSchema `json:"outputSchema"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败: "+err.Error(), nil)
			return
		}
	}

	def := dsl.Definition{}
	if req.Definition != nil {
		def = *req.Definition
		if def.RulesetID == "" {
			def.RulesetID = r.PathValue("id")
		}
	} else {
		stored, ok := s.deps.Rulesets.Get(r.PathValue("id"))
		if !ok {
			fail(w, r, &dsl.NotExistError{What: "规则集", ID: r.PathValue("id")})
			return
		}
		def = stored
	}

	writeJSON(w, http.StatusOK, s.analyze(def, req.OutputSchema))
}

// analyze assembles one dry-run: the fact catalog uses the current catalog snapshot, and the output schema is picked by precedence.
func (s *Server) analyze(def dsl.Definition, override *contracts.OutputSchema) dsl.Report {
	out := override
	if out == nil {
		out = def.OutputSchema
	}
	cat := s.deps.Facts.Snapshot()
	return dsl.Analyze(dsl.Input{
		Definition:   def,
		Catalog:      cat,
		OutputSchema: out,
		Limits:       dsl.DefaultLimits(),
	})
}

// factsInUse backs the facts package's delete protection: which rulesets reference a fact.
func (s *Server) factsInUse(fact string) []string {
	if s.deps.Rulesets == nil {
		return nil
	}
	return s.deps.Rulesets.ReferencingFact(fact)
}
