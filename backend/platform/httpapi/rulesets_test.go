package httpapi

import (
	"net/http"
	"testing"
)

const factForRule = `{
  "fact": "request.amount", "type": "decimal", "unit": "CNY",
  "required": true, "semantics": "本次提款金额", "version": "v1"
}`

const goodRuleset = `{
  "rulesetId": "rs-amount-limit",
  "scene": "withdraw-risk-check",
  "version": "rs-1",
  "hitStrategy": "ALL",
  "defaultDecision": "REJECT",
  "rules": [
    {
      "ruleId": "R1",
      "priority": 100,
      "when": "request.amount > 1000",
      "then": {"decision": "REJECT", "reasonCode": "AMOUNT_LIMIT_EXCEEDED"}
    }
  ]
}`

// A PUT must pass dry-run first: a ruleset referencing a missing field cannot be stored.
func TestPutRulesetRejectsBadField(t *testing.T) {
	h := newTestServer(t)
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}

	bad := `{
	  "rulesetId": "rs-amount-limit", "scene": "withdraw-risk-check",
	  "defaultDecision": "REJECT",
	  "rules": [{"ruleId": "R1", "priority": 100,
	    "when": {"op":"gt","args":[{"fact":"request.amout"},{"type":"decimal","value":"1000"}]},
	    "then": {"decision":"REJECT","reasonCode":"AMOUNT_LIMIT_EXCEEDED"}}]
	}`
	rec, body := do(t, h, "PUT", "/api/v1/rulesets/rs-amount-limit", bad)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("期望 422，实际 %d %s", rec.Code, rec.Body)
	}
	errs, _ := body["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("缺诊断: %s", rec.Body)
	}
	first := errs[0].(map[string]any)
	if first["code"] != "DSL_FIELD_NOT_FOUND" {
		t.Fatalf("code = %v", first["code"])
	}
	if first["path"] != "rules[0].when.args[0]" {
		t.Fatalf("path = %v", first["path"])
	}

	// not persisted
	rec, _ = do(t, h, "GET", "/api/v1/rulesets/rs-amount-limit", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非法规则集不应入库，实际 %d", rec.Code)
	}
}

func TestPutValidateAndDeleteRuleset(t *testing.T) {
	h := newTestServer(t)
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}

	rec, body := do(t, h, "PUT", "/api/v1/rulesets/rs-amount-limit", goodRuleset)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	report := body["report"].(map[string]any)
	if report["valid"] != true {
		t.Fatalf("report 应有效: %s", rec.Body)
	}
	stats := report["stats"].(map[string]any)
	if int(stats["rules"].(float64)) != 1 || int(stats["slots"].(float64)) != 1 {
		t.Fatalf("stats = %v", stats)
	}

	rec, body = do(t, h, "GET", "/api/v1/rulesets?scene=withdraw-risk-check", "")
	if rec.Code != http.StatusOK || int(body["total"].(float64)) != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	// dry-run of an unsaved edit: nothing is written to the store
	draft := `{"definition":{"rulesetId":"rs-amount-limit","scene":"withdraw-risk-check",
	  "defaultDecision":"REJECT","rules":[{"ruleId":"R9","priority":10,
	  "when":{"op":"unknownOp","args":[{"fact":"request.amount"}]},
	  "then":{"decision":"APPROVE","reasonCode":"OK"}}]}}`
	rec, body = do(t, h, "POST", "/api/v1/rulesets/rs-amount-limit/validate", draft)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate = %d %s", rec.Code, rec.Body)
	}
	if body["valid"] != false {
		t.Fatalf("草稿应判为不合法: %s", rec.Body)
	}
	if _, ok := body["errors"].([]any); !ok {
		t.Fatalf("缺 errors: %s", rec.Body)
	}

	rec, _ = do(t, h, "DELETE", "/api/v1/rulesets/rs-amount-limit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
}

// B6 leftover: a fact that a ruleset references must not be deleted.
func TestFactDeleteBlockedByRuleset(t *testing.T) {
	h := newTestServer(t)
	if rec, _ := do(t, h, "POST", "/api/v1/facts", factForRule); rec.Code != http.StatusCreated {
		t.Fatalf("create fact = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, "PUT", "/api/v1/rulesets/rs-amount-limit", goodRuleset); rec.Code != http.StatusOK {
		t.Fatalf("put ruleset = %d %s", rec.Code, rec.Body)
	}

	rec, body := do(t, h, "DELETE", "/api/v1/facts/request.amount", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("期望 409，实际 %d %s", rec.Code, rec.Body)
	}
	if body["errorCode"] != "FACT_IN_USE" {
		t.Fatalf("errorCode = %v", body["errorCode"])
	}
	details, _ := body["details"].(map[string]any)
	referers, _ := details["referers"].([]any)
	if len(referers) != 1 || referers[0] != "rs-amount-limit" {
		t.Fatalf("referers = %v", details)
	}

	// once the ruleset is deleted the fact can be deleted
	if rec, _ := do(t, h, "DELETE", "/api/v1/rulesets/rs-amount-limit", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete ruleset = %d", rec.Code)
	}
	if rec, _ := do(t, h, "DELETE", "/api/v1/facts/request.amount", ""); rec.Code != http.StatusOK {
		t.Fatalf("解除引用后应可删除，实际 %d %s", rec.Code, rec.Body)
	}
}
