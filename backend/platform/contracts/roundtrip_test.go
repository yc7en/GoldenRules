package contracts

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// B0 acceptance: the same fixture is byte-equivalent after a round trip on the Go side — i.e. the contract JSON written by Rust
// can be parsed by Go without losing fields. This test plus its Rust counterpart is the only referee for cross-language consistency.
func fixtures(t *testing.T) string {
	t.Helper()
	return "../../fixtures"
}

// assertSameJSON decides "semantic equivalence" of contract JSON.
//
// Why bytes are not compared: key order / omission styles necessarily differ across languages (Go emits struct field order,
// and omitempty drops false/empty values), so contract consistency can only be decided by value, not by text.
func assertSameJSON(t *testing.T, want, got []byte, label string) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: unmarshal want: %v", label, err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: unmarshal got: %v", label, err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("%s round-trip mismatch\n in: %s\nout: %s", label, want, got)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixtures(t) + "/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func TestRulePackageRoundTrip(t *testing.T) {
	raw := readFixture(t, "rule-package.json")
	var pkg RulePackage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("unmarshal RulePackage: %v", err)
	}
	if pkg.PackageID != "pkg-withdraw-risk" || pkg.PackageVersion != "v12" {
		t.Fatalf("unexpected identity: %s/%s", pkg.PackageID, pkg.PackageVersion)
	}
	if len(pkg.Rulesets) != 4 {
		t.Fatalf("want 4 rulesets, got %d", len(pkg.Rulesets))
	}
	rules := 0
	for _, rs := range pkg.Rulesets {
		rules += len(rs.Rules)
		if rs.HitStrategy == "" {
			t.Fatalf("ruleset %s lost hitStrategy", rs.RulesetID)
		}
	}
	if rules != 9 {
		t.Fatalf("want 9 rules, got %d", rules)
	}
	out, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSameJSON(t, raw, out, "RulePackage")
}

func TestFactCatalogRoundTrip(t *testing.T) {
	raw := readFixture(t, "fact-catalog.json")
	var catalog FactCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("unmarshal FactCatalog: %v", err)
	}
	if len(catalog.Facts) != 8 {
		t.Fatalf("want 8 facts, got %d", len(catalog.Facts))
	}
	out, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSameJSON(t, raw, out, "FactCatalog")
}

func TestSnapshotAndDecisionRoundTrip(t *testing.T) {
	raw := readFixture(t, "snapshot.json")
	var snap FactSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal FactSnapshot: %v", err)
	}
	var amount string
	if err := json.Unmarshal(snap.Facts["request.amount"], &amount); err != nil {
		t.Fatalf("amount must stay a decimal string, not a float: %v", err)
	}
	if amount != "1500" {
		t.Fatalf("want 1500, got %s", amount)
	}

	// decision result: decimals inside output values must also be strings (no precision loss across languages)
	dec := []byte(`{"resultVersion":"v1","snapshotId":"fs-1","traceId":"t-1","rulesetId":"rs-1",
		"rulesetVersion":"rs-v1","decision":"REJECT","reasonCode":"AMOUNT_LIMIT_EXCEEDED",
		"hitRules":[{"ruleId":"R2001","priority":100,"reasonCode":"AMOUNT_LIMIT_EXCEEDED"}],
		"evaluatedRules":3,"outputs":{"excessAmount":"500.10"},"degraded":false,
		"degradedReasons":[],"executionTimeMs":0.4}`)
	var res DecisionResult
	if err := json.Unmarshal(dec, &res); err != nil {
		t.Fatalf("unmarshal DecisionResult: %v", err)
	}
	var got string
	if err := json.Unmarshal(res.Outputs["excessAmount"], &got); err != nil {
		t.Fatalf("output decimal must be string: %v", err)
	}
	if got != "500.10" {
		t.Fatalf("want 500.10, got %s", got)
	}
}
