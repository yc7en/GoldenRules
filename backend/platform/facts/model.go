// Package facts implements B6: fact catalog (Fact Catalog) management.
//
// Contract basis: `contracts/02` (fact contract).
//
// # Single source of truth
//
// The top-level metadata (`FactSchema`) is the **only source of truth**; the JSON Schema and the slot table are **artifacts**
// generated idempotently from that metadata (`contracts/02` §1.1). So this package never "reads an artifact back and edits it";
// it only does "change the source of truth → regenerate → compare with the artifact on disk to see whether it has gone stale".
//
// # Boundaries
//
// It stores contracts only (field name/type/semantics/freshness/redaction/version) and **does not store fetch logic** — fetching belongs to the
// external Fact Provider / Context Builder (`ARCHITECTURE.md` §2.2 data supply boundary).
package facts

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"goldenrules/platform/contracts"
)

// Fact catalog validation error codes. Level error = blocks saving, warn = allowed through but recorded (`contracts/05` §6.2 failure strategy).
const (
	CodeNameInvalid          = "FACT_NAME_INVALID"
	CodeDuplicate            = "FACT_DUPLICATE"
	CodeTypeInvalid          = "FACT_TYPE_INVALID"
	CodeRequiredDefault      = "FACT_REQUIRED_DEFAULT_CONFLICT"
	CodeDefaultNull          = "FACT_DEFAULT_NULL"
	CodeDefaultNotAllowed    = "FACT_DEFAULT_NOT_ALLOWED"
	CodeDefaultTypeMismatch  = "FACT_DEFAULT_TYPE_MISMATCH"
	CodeEnumRequired         = "FACT_ENUM_REQUIRED"
	CodeEnumDuplicate        = "FACT_ENUM_DUPLICATE"
	CodeEnumNotAllowed       = "FACT_ENUM_NOT_ALLOWED"
	CodeSensitivityInvalid   = "FACT_SENSITIVITY_INVALID"
	CodeConsistencyInvalid   = "FACT_CONSISTENCY_INVALID"
	CodeOnMissingInvalid     = "FACT_ONMISSING_INVALID"
	CodeVersionInvalid       = "FACT_VERSION_INVALID"
	CodeFreshnessInvalid     = "FACT_FRESHNESS_INVALID"
	CodeUnitMissing          = "FACT_UNIT_MISSING"
	CodeDeprecatedNoReplace  = "FACT_DEPRECATED_NO_REPLACEMENT"
	CodeReplacedByUnknown    = "FACT_REPLACED_BY_UNKNOWN"
	CodeNotFound             = "FACT_NOT_FOUND"
	CodeInUse                = "FACT_IN_USE"
	CodeArtifactMissing      = "FACT_ARTIFACT_MISSING"
	CodeArtifactStale        = "FACT_ARTIFACT_STALE"
	CodeCatalogVersionFormat = "FACT_CATALOG_VERSION_INVALID"
)

// Fact name: `namespace.field`, lower-case namespace + underscore, camelCase field name (`contracts/02` §1.3 example).
var factNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-zA-Z][a-zA-Z0-9_]*$`)

var factVersionPattern = regexp.MustCompile(`^v[0-9]+$`)

// Allowed fact types (`contracts/01` §3.3).
var allowedKinds = map[string]bool{
	"integer": true,
	"decimal": true,
	"string":  true,
	"boolean": true,
	"enum":    true,
	"array":   true,
}

var allowedSensitivity = map[string]bool{"PII": true, "INTERNAL": true, "PUBLIC": true}
var allowedConsistency = map[string]bool{"STRONG": true, "EVENTUAL": true}
var allowedOnMissing = map[string]bool{"ERROR": true, "DEFAULT": true, "SKIP": true, "FALLBACK_DECISION": true}
var allowedOnStale = map[string]bool{"DEGRADE": true, "DEFAULT": true, "ERROR": true}

// Issue is one validation/governance diagnostic. It has the same shape as the diagnostics in `contracts/01` §3.7:
// error code + field path + suggestion, so the agent can self-correct from it.
type Issue struct {
	Code    string `json:"code"`
	Level   string `json:"level"` // error | warn
	Path    string `json:"path"`  // e.g. facts[3].default
	Fact    string `json:"fact,omitempty"`
	Message string `json:"message"`
	Suggest string `json:"suggest,omitempty"`
}

// HasError reports whether a batch of diagnostics contains a blocking issue.
func HasError(issues []Issue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

// ValidationError wraps "validation failed", so the HTTP layer can tell "bad request" from "internal error".
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 0 {
		return "事实校验未通过"
	}
	return fmt.Sprintf("事实校验未通过: %s %s", e.Issues[0].Code, e.Issues[0].Message)
}

// NotFoundError means the fact does not exist.
type NotFoundError struct{ Fact string }

func (e *NotFoundError) Error() string { return "事实不存在: " + e.Fact }

// InUseError means the fact is still referenced by rule sets, so deletion is refused.
type InUseError struct {
	Fact     string
	Referers []string
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("事实 %s 仍被 %d 个规则集引用: %s", e.Fact, len(e.Referers), strings.Join(e.Referers, ", "))
}

// Catalog is the fact catalog document (single source of truth + revision number).
type Catalog struct {
	CatalogVersion string                 `json:"catalogVersion"`
	Revision       int64                  `json:"revision"`
	UpdatedAt      string                 `json:"updatedAt"`
	UpdatedBy      string                 `json:"updatedBy,omitempty"`
	Facts          []contracts.FactSchema `json:"facts"`
}

// Normalize fills in defaults. The defaults themselves are **part of the contract** (`contracts/05` §4.1),
// so they are filled in once here instead of having every downstream consumer guess.
func Normalize(in contracts.FactSchema) contracts.FactSchema {
	out := in
	out.Fact = strings.TrimSpace(out.Fact)
	out.Type = strings.ToLower(strings.TrimSpace(out.Type))
	if out.Version == "" {
		out.Version = "v1"
	}
	if out.Sensitivity == "" {
		out.Sensitivity = "INTERNAL"
	}
	if out.Consistency == "" {
		out.Consistency = "STRONG"
	}
	if out.OnMissing == "" {
		out.OnMissing = "ERROR"
	}
	if out.Freshness != nil && out.Freshness.OnStale == "" {
		f := *out.Freshness
		f.OnStale = "DEGRADE"
		out.Freshness = &f
	}
	return out
}

// Validate validates a single fact contract.
//
// existing is the catalog index "excluding the fact itself", used for duplicate checks and replacedBy validation;
// pass the whole index when creating and the index without the fact itself when updating.
func Validate(in contracts.FactSchema, existing map[string]contracts.FactSchema) []Issue {
	var issues []Issue
	add := func(level, code, path, msg string, suggest ...string) {
		s := ""
		if len(suggest) > 0 {
			s = suggest[0]
		}
		issues = append(issues, Issue{Code: code, Level: level, Path: path, Fact: in.Fact, Message: msg, Suggest: s})
	}

	// §1 field name
	if !factNamePattern.MatchString(in.Fact) {
		add("error", CodeNameInvalid, "fact", "事实名必须形如 namespace.field（命名空间小写，字段名小驼峰）",
			"例如 request.amount、customer.withdrawCount24h")
	}

	// §2 type
	if !allowedKinds[in.Type] {
		add("error", CodeTypeInvalid, "type",
			fmt.Sprintf("不支持的类型 %q", in.Type),
			"合法值：integer / decimal / string / boolean / enum / array（contracts/01 §3.3）")
	}

	// §1.2 the three orthogonal field states
	if in.Required && in.HasDefault {
		add("error", CodeRequiredDefault, "required",
			"required 与 hasDefault 互斥：必填字段不能同时声明默认值",
			"要么 required=true 去掉默认值，要么 required=false 保留默认值")
	}
	if in.HasDefault {
		if len(in.Default) == 0 || string(in.Default) == "null" {
			add("error", CodeDefaultNull, "default",
				"hasDefault=true 但 default 为 null；default: null 视为语义矛盾",
				"给 default 一个与类型匹配的值，或把 hasDefault 改为 false 并省略 default")
		} else if err := checkDefault(in.Type, in.Default, in.Enum); err != nil {
			add("error", CodeDefaultTypeMismatch, "default", err.Error(),
				"default 的类型必须与 type 一致")
		}
	} else if len(in.Default) > 0 && string(in.Default) != "null" {
		add("error", CodeDefaultNotAllowed, "default",
			"未声明 hasDefault 时不允许出现 default 字段",
			"补上 hasDefault=true，或删除 default")
	}

	// §3 enum
	if in.Type == "enum" {
		if len(in.Enum) == 0 {
			add("error", CodeEnumRequired, "enum", "enum 类型必须给出枚举值列表", "例如 [\"CNY\",\"USD\"]")
		}
		seen := map[string]bool{}
		for i, v := range in.Enum {
			if v == "" {
				add("error", CodeEnumRequired, fmt.Sprintf("enum[%d]", i), "枚举值不能为空字符串")
				continue
			}
			if seen[v] {
				add("error", CodeEnumDuplicate, fmt.Sprintf("enum[%d]", i),
					fmt.Sprintf("枚举值 %q 重复", v), "删除重复项")
			}
			seen[v] = true
		}
	} else if len(in.Enum) > 0 {
		add("error", CodeEnumNotAllowed, "enum",
			"只有 type=enum 才允许声明 enum 列表", "把 type 改为 enum，或删除 enum")
	}

	// §9 governance fields
	if !allowedSensitivity[in.Sensitivity] {
		add("error", CodeSensitivityInvalid, "sensitivity",
			fmt.Sprintf("不支持的脱敏级别 %q", in.Sensitivity), "合法值：PII / INTERNAL / PUBLIC")
	}
	if !allowedConsistency[in.Consistency] {
		add("error", CodeConsistencyInvalid, "consistency",
			fmt.Sprintf("不支持的一致性 %q", in.Consistency), "合法值：STRONG / EVENTUAL")
	}
	if !allowedOnMissing[in.OnMissing] {
		add("error", CodeOnMissingInvalid, "onMissing",
			fmt.Sprintf("不支持的缺失策略 %q", in.OnMissing),
			"合法值：ERROR / DEFAULT / SKIP / FALLBACK_DECISION（contracts/05 §4.1）")
	}
	if !factVersionPattern.MatchString(in.Version) {
		add("error", CodeVersionInvalid, "version",
			fmt.Sprintf("版本号 %q 不合法", in.Version), "形如 v1、v2")
	}
	if in.Freshness != nil {
		if in.Freshness.MaxAgeSec < 0 {
			add("error", CodeFreshnessInvalid, "freshness.maxAgeSec", "maxAgeSec 不能为负")
		}
		if !allowedOnStale[in.Freshness.OnStale] {
			add("error", CodeFreshnessInvalid, "freshness.onStale",
				fmt.Sprintf("不支持的过期策略 %q", in.Freshness.OnStale), "合法值：DEGRADE / DEFAULT / ERROR")
		}
	}

	// Business reminders (warn, not blocking)
	if in.Type == "decimal" && in.Unit == "" {
		add("warn", CodeUnitMissing, "unit",
			"decimal 字段未声明 unit；金额必须能在语义上确定单位（contracts/02 §2）",
			"例如 unit: CNY")
	}
	if in.Deprecated && in.ReplacedBy == "" {
		add("warn", CodeDeprecatedNoReplace, "replacedBy",
			"字段已废弃但未给出替换字段", "填 replacedBy，方便消费方迁移（contracts/02 §7）")
	}
	if in.ReplacedBy != "" {
		if _, ok := existing[in.ReplacedBy]; !ok {
			add("warn", CodeReplacedByUnknown, "replacedBy",
				fmt.Sprintf("replacedBy 指向的字段 %s 不在目录中", in.ReplacedBy), "先创建替换字段")
		}
	}
	if _, dup := existing[in.Fact]; dup {
		add("error", CodeDuplicate, "fact", "事实名已存在", "改用 Update 修改已有事实")
	}

	return issues
}

// checkDefault validates that the default value matches the declared type (no implicit cross-type conversion).
func checkDefault(kind string, raw json.RawMessage, enum []string) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("default 不是合法 JSON: %v", err)
	}
	switch kind {
	case "integer":
		switch t := v.(type) {
		case float64:
			if t != float64(int64(t)) {
				return fmt.Errorf("integer 类型的 default 不能是小数: %v", t)
			}
			return nil
		case string:
			if _, err := parseInt(t); err != nil {
				return fmt.Errorf("integer 类型的 default 无法解析: %q", t)
			}
			return nil
		}
		return fmt.Errorf("integer 类型的 default 期望整数，得到 %T", v)
	case "decimal":
		switch t := v.(type) {
		case float64:
			return nil
		case string:
			if _, err := parseDecimal(t); err != nil {
				return fmt.Errorf("decimal 类型的 default 无法解析: %q", t)
			}
			return nil
		}
		return fmt.Errorf("decimal 类型的 default 期望字符串或数字，得到 %T", v)
	case "string", "enum":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s 类型的 default 期望字符串，得到 %T", kind, v)
		}
		if kind == "enum" && len(enum) > 0 && !contains(enum, s) {
			return fmt.Errorf("default %q 不在枚举值中", s)
		}
		return nil
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("boolean 类型的 default 期望 true/false，得到 %T", v)
		}
		return nil
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("array 类型的 default 期望数组，得到 %T", v)
		}
		return nil
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// CatalogVersion builds the catalog version: `catalog-vYYYY.MM.<revision>`.
//
// The month is there because the contract examples use `catalog-v2026.10`; the revision number is there so that "several changes within the same month"
// still each get a unique version (rule packages lock `factSchemaVersion`, so the version must be unique and monotonic).
func CatalogVersion(now time.Time, revision int64) string {
	return fmt.Sprintf("catalog-v%s.%d", now.UTC().Format("2006.01"), revision)
}

// SortFacts sorts by fact name ascending — the **one deterministic order** inside the catalog, which every artifact follows.
func SortFacts(list []contracts.FactSchema) {
	sort.Slice(list, func(i, j int) bool { return list[i].Fact < list[j].Fact })
}

// Index builds the "fact name → contract" index.
func Index(list []contracts.FactSchema) map[string]contracts.FactSchema {
	out := make(map[string]contracts.FactSchema, len(list))
	for _, f := range list {
		out[f.Fact] = f
	}
	return out
}
