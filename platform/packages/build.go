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

// BuildInput 组装一个规则包版本所需的全部输入。
type BuildInput struct {
	PackageID string
	Version   string // 留空 = 由仓库决定下一个版本号
	Rulesets  []dsl.Definition
	Catalog   contracts.FactCatalog
	Actor     string
	Changelog string
	RiskLevel string
	Now       time.Time
}

// Build 编译并组装规则包（不落库）。
//
// 返回的错误是**聚合**的：任何一个规则集 dry-run 不过，整个包都不成立
// （`contracts/01` §6「原子性」：一次发布 = 整个包的不可变快照，不能部分上线）。
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

	// 签名先算（覆盖内容），再写回包体——包体里的 signature 与 Version.Signature 必须同一值，
	// 否则引擎拿到的产物验签会失败。
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

// Sign 计算整包摘要：除签名外的全部内容参与哈希。
//
// P0 用 sha256（`contracts/01` §6「P1 前可先 hash，P1 加签名」）。
// 摘要覆盖 `metadata` —— 发布人与时间也是审计对象，改了就应被发现。
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

// Verify 校验包内容与签名是否一致（防篡改：改一个字符签名就不匹配）。
func Verify(v Version) bool {
	if v.Signature == "" {
		return false
	}
	return Sign(v.Package) == v.Signature
}

// NextVersion 计算下一个版本号。
//
// 兼容两种既有写法：fixture 与 `contracts/06` 附录 B 用 `v12` 这类整数递增，
// `contracts/06` §2.2 又要求语义化 `MAJOR.MINOR.PATCH`。两者都支持：
// 全是 `vN` 就 `v(N+1)`，否则按 PATCH 递增。
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

	// 语义化：取最大版本再递增 PATCH
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
