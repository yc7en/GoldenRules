package packages

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
)

// BuildInput carries every input needed to assemble one rule package version.
type BuildInput struct {
	PackageID string
	Version   string // empty = the store assigns the next version
	Rulesets  []dsl.Definition
	Catalog   contracts.FactCatalog
	Actor     string
	Changelog string
	RiskLevel string
	Now       time.Time
}

// Build compiles and assembles the rule package (nothing is persisted).
//
// The returned error is **aggregated**: if any single ruleset fails dry-run, the whole package fails
// (`contracts/01` §6 "atomicity": one publish = an immutable snapshot of the whole package, no partial rollout).
func Build(in BuildInput) (Version, []dsl.Report, error) {
	if strings.TrimSpace(in.PackageID) == "" {
		return Version{}, nil, &StateError{Code: CodeEmptyPackage, Message: "缺少 packageId"}
	}
	if len(in.Rulesets) == 0 {
		return Version{}, nil, &StateError{Code: CodeEmptyPackage, Message: "规则包至少包含一个规则集"}
	}

	var (
		reports   []dsl.Report
		bad       []string
		compiled  []contracts.RuleSet
		schemas   []contracts.OutputSchema
		tests     []contracts.TestCase
		hashes    = map[string]string{}
		ids       []string
		ruleSum   int
		pending   int
		confirmed int
	)

	for _, def := range in.Rulesets {
		rep := dsl.Analyze(dsl.Input{
			Definition:   def,
			Catalog:      in.Catalog,
			OutputSchema: def.OutputSchema,
			Limits:       dsl.DefaultLimits(),
		})
		reports = append(reports, rep)
		if !rep.Valid {
			for _, e := range rep.Errors {
				bad = append(bad, fmt.Sprintf("%s: %s %s %s", def.RulesetID, e.Code, e.Path, e.Message))
			}
			continue
		}
		compiled = append(compiled, *rep.IR)
		hashes[def.RulesetID] = rep.SourceHash
		ids = append(ids, def.RulesetID)
		ruleSum += len(rep.IR.Rules)

		if def.OutputSchema != nil {
			schemas = append(schemas, *def.OutputSchema)
		}
		for _, tc := range def.Tests {
			tests = append(tests, tc)
			if tc.Status == "approved" {
				confirmed++
			} else {
				pending++
			}
		}
	}
	if len(bad) > 0 {
		return Version{}, reports, &StateError{Code: CodeCompileFailed, Message: "规则包编译失败：" + strings.Join(bad, "；")}
	}

	sort.Strings(ids)
	sort.Slice(compiled, func(i, j int) bool { return compiled[i].RulesetID < compiled[j].RulesetID })
	schemas = dedupeSchemas(schemas)
	sort.Slice(tests, func(i, j int) bool { return tests[i].TestID < tests[j].TestID })

	pkg := contracts.RulePackage{
		PackageID:         in.PackageID,
		PackageVersion:    in.Version,
		FactSchemaVersion: in.Catalog.CatalogVersion,
		DSLVersion:        contracts.DSLVersion,
		IRVersion:         contracts.IRVersion,
		Rulesets:          compiled,
		Tests:             tests,
		OutputSchemas:     schemas,
		Metadata: contracts.PackageMetadata{
			CreatedAt: in.Now.UTC().Format(time.RFC3339),
			CreatedBy: in.Actor,
			RiskLevel: in.RiskLevel,
			Changelog: in.Changelog,
		},
	}

	// The signature is computed first (covering the content) and then written back into the package body — the signature inside the body and Version.Signature must be the same value,
	// otherwise signature verification of the artifact the engine receives would fail.
	sig := Sign(pkg)
	pkg.Signature = sig

	return Version{
		PackageID:         in.PackageID,
		PackageVersion:    in.Version,
		FactSchemaVersion: in.Catalog.CatalogVersion,
		DSLVersion:        contracts.DSLVersion,
		IRVersion:         contracts.IRVersion,
		Status:            StatusDraft,
		RiskLevel:         in.RiskLevel,
		Changelog:         in.Changelog,
		CreatedBy:         in.Actor,
		CreatedAt:         in.Now.UTC().Format(time.RFC3339),
		Signature:         sig,
		RuleCount:         ruleSum,
		SourceHashes:      hashes,
		RulesetIDs:        ids,
		UnconfirmedTC:     pending,
		ConfirmedTC:       confirmed,
		Package:           pkg,
	}, reports, nil
}

// Sign computes the whole-package digest: everything except the signature is hashed.
//
// P0 uses sha256 (`contracts/01` §6 "hashing is acceptable before P1; signing lands in P1").
// The digest covers `metadata` — the publisher and the timestamp are audit subjects too, so tampering with them must be detected.
func Sign(pkg contracts.RulePackage) string {
	clone := pkg
	clone.Signature = ""
	body, err := json.Marshal(clone)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Verify checks that the package content matches the signature (tamper detection: changing a single character breaks the match).
func Verify(v Version) bool {
	if v.Signature == "" {
		return false
	}
	return Sign(v.Package) == v.Signature
}

// NextVersion computes the next version number.
//
// Two existing conventions are supported: fixtures and `contracts/06` appendix B use integer increments like `v12`,
// while `contracts/06` §2.2 requires semver `MAJOR.MINOR.PATCH`. Both work:
// if everything is `vN` it returns `v(N+1)`, otherwise it increments PATCH.
func NextVersion(existing []string) string {
	if len(existing) == 0 {
		return "v1"
	}
	allIntStyle := true
	maxN := 0
	for _, v := range existing {
		n, ok := parseIntStyle(v)
		if !ok {
			allIntStyle = false
			break
		}
		if n > maxN {
			maxN = n
		}
	}
	if allIntStyle {
		return "v" + strconv.Itoa(maxN+1)
	}

	// semver: take the highest version and increment PATCH
	var best [3]int
	var bestRaw string
	for _, v := range existing {
		cur, ok := parseSemver(v)
		if !ok {
			continue
		}
		if bestRaw == "" || lessSemver(best, cur) {
			best, bestRaw = cur, v
		}
	}
	if bestRaw == "" {
		return "v1"
	}
	return fmt.Sprintf("%d.%d.%d", best[0], best[1], best[2]+1)
}

func parseIntStyle(v string) (int, bool) {
	if !strings.HasPrefix(v, "v") {
		return 0, false
	}
	n, err := strconv.Atoi(v[1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func parseSemver(v string) ([3]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func lessSemver(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func dedupeSchemas(in []contracts.OutputSchema) []contracts.OutputSchema {
	seen := map[string]bool{}
	out := make([]contracts.OutputSchema, 0, len(in))
	for _, s := range in {
		key := s.OutputSchemaID + "@" + s.OutputSchemaVersion
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OutputSchemaID != out[j].OutputSchemaID {
			return out[i].OutputSchemaID < out[j].OutputSchemaID
		}
		return out[i].OutputSchemaVersion < out[j].OutputSchemaVersion
	})
	return out
}
