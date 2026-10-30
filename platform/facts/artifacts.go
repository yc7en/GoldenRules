package facts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"goldenrules/platform/contracts"
)

// 生成物文件名。三个文件都由顶层元数据幂等生成（`contracts/02` §1.1）：
//
//	catalog.json     —— 真源的规范化导出（供引擎与 fixtures 直接消费）
//	fact-schema.json —— JSON Schema 视图（供入口校验与人读）
//	slot-table.json  —— 全目录 slot 索引（字段名 → 目录内编号）
//
// 注意：规则集内的 slot 表是**另一张表**（编号在规则集内独立），由 B8 编译时生成；
// 这里这张是「目录级」的，两者只在「字段名 → 类型」上必须一致。
const (
	FileCatalog    = "catalog.json"
	FileJSONSchema = "fact-schema.json"
	FileSlotTable  = "slot-table.json"
)

// ArtifactFile 是一个生成物文件。
type ArtifactFile struct {
	Name   string
	Body   []byte
	Digest string
}

// Artifacts 是一批生成物 + 目录级 slot 表。
type Artifacts struct {
	CatalogVersion string
	Files          []ArtifactFile
	SlotTable      []contracts.SlotDef
}

// File 按名取生成物。
func (a Artifacts) File(name string) (ArtifactFile, bool) {
	for _, f := range a.Files {
		if f.Name == name {
			return f, true
		}
	}
	return ArtifactFile{}, false
}

// Drift 描述「磁盘上的生成物与重新生成的结果不一致」。
type Drift struct {
	File    string `json:"file"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Artifacts 重新生成全部生成物。纯函数：同一份目录必产同一份字节（可 diff、可 CI 卡门禁）。
func (s *Service) Artifacts() (Artifacts, error) {
	cat := s.Catalog()
	if cat.CatalogVersion == "" {
		return Artifacts{}, fmt.Errorf("事实目录版本为空，无法生成产物")
	}
	SortFacts(cat.Facts)

	out := Artifacts{CatalogVersion: cat.CatalogVersion}

	// 1) 真源导出
	body, err := marshalCanonical(contracts.FactCatalog{CatalogVersion: cat.CatalogVersion, Facts: cat.Facts})
	if err != nil {
		return Artifacts{}, err
	}
	out.Files = append(out.Files, newFile(FileCatalog, body))

	// 2) JSON Schema
	body, err = marshalCanonical(buildJSONSchema(cat))
	if err != nil {
		return Artifacts{}, err
	}
	out.Files = append(out.Files, newFile(FileJSONSchema, body))

	// 3) 目录级 slot 表
	out.SlotTable = buildSlotTable(cat.Facts)
	body, err = marshalCanonical(slotTableDoc{
		Scope:          "catalog",
		CatalogVersion: cat.CatalogVersion,
		Slots:          out.SlotTable,
	})
	if err != nil {
		return Artifacts{}, err
	}
	out.Files = append(out.Files, newFile(FileSlotTable, body))

	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out, nil
}

// Write 把生成物写到目录，**内容相同则不落盘**（幂等：重复执行不产生 diff）。
// 返回真正被改写的文件名。
func (s *Service) Write(dir string) ([]string, error) {
	arts, err := s.Artifacts()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建产物目录失败: %w", err)
	}
	var changed []string
	for _, f := range arts.Files {
		path := filepath.Join(dir, f.Name)
		if old, err := os.ReadFile(path); err == nil && string(old) == string(f.Body) {
			continue
		}
		if err := os.WriteFile(path, f.Body, 0o644); err != nil {
			return changed, fmt.Errorf("写 %s 失败: %w", f.Name, err)
		}
		changed = append(changed, f.Name)
	}
	return changed, nil
}

// Verify 校验磁盘生成物是否过期（CI 门禁：重新生成后 diff 必须为空）。
// 返回空切片 = 未过期。它比对的是**字节**，不是「两处是否语义一致」——后者治标（`contracts/02` §1.1）。
func (s *Service) Verify(dir string) ([]Drift, error) {
	arts, err := s.Artifacts()
	if err != nil {
		return nil, err
	}
	var drifts []Drift
	for _, f := range arts.Files {
		path := filepath.Join(dir, f.Name)
		old, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			drifts = append(drifts, Drift{
				File:    f.Name,
				Code:    CodeArtifactMissing,
				Message: "生成物缺失：" + f.Name,
			})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读 %s 失败: %w", f.Name, err)
		}
		if string(old) != string(f.Body) {
			drifts = append(drifts, Drift{
				File:    f.Name,
				Code:    CodeArtifactStale,
				Message: "生成物已过期：重新生成后与磁盘内容不同（事实目录改过但没重新生成）",
			})
		}
	}
	return drifts, nil
}

func newFile(name string, body []byte) ArtifactFile {
	sum := sha256.Sum256(body)
	return ArtifactFile{Name: name, Body: body, Digest: "sha256:" + hex.EncodeToString(sum[:])}
}

// marshalCanonical 产出确定性的 JSON：结构体字段顺序固定、map 键由 encoding/json 排序、
// 末尾补换行（便于 git diff 与文本工具）。
func marshalCanonical(v any) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// ---------------------------------------------------------------------------
// JSON Schema 视图
// ---------------------------------------------------------------------------

type jsonSchemaDoc struct {
	Schema               string                    `json:"$schema"`
	ID                   string                    `json:"$id"`
	Title                string                    `json:"title"`
	Type                 string                    `json:"type"`
	CatalogVersion       string                    `json:"x-catalog-version"`
	Properties           map[string]jsonSchemaProp `json:"properties"`
	Required             []string                  `json:"required,omitempty"`
	AdditionalProperties bool                      `json:"additionalProperties"`
}

type jsonSchemaProp struct {
	Type        string               `json:"type"`
	Enum        []string             `json:"enum,omitempty"`
	Format      string               `json:"format,omitempty"`
	Unit        string               `json:"x-unit,omitempty"`
	Semantics   string               `json:"description,omitempty"`
	Consistency string               `json:"x-consistency,omitempty"`
	Sensitivity string               `json:"x-sensitivity,omitempty"`
	FactVersion string               `json:"x-fact-version,omitempty"`
	OnMissing   string               `json:"x-on-missing,omitempty"`
	Nullable    bool                 `json:"x-nullable,omitempty"`
	HasDefault  bool                 `json:"x-has-default,omitempty"`
	Default     json.RawMessage      `json:"default,omitempty"`
	Freshness   *contracts.Freshness `json:"x-freshness,omitempty"`
	Deprecated  bool                 `json:"x-deprecated,omitempty"`
	ReplacedBy  string               `json:"x-replaced-by,omitempty"`
	Source      string               `json:"x-source,omitempty"`
}

func buildJSONSchema(cat Catalog) jsonSchemaDoc {
	doc := jsonSchemaDoc{
		Schema:               "https://json-schema.org/draft/2020-12/schema",
		ID:                   "goldenrules/fact-catalog/" + cat.CatalogVersion,
		Title:                "GoldenRules Fact Catalog",
		Type:                 "object",
		CatalogVersion:       cat.CatalogVersion,
		Properties:           make(map[string]jsonSchemaProp, len(cat.Facts)),
		AdditionalProperties: false,
	}
	for _, f := range cat.Facts {
		doc.Properties[f.Fact] = jsonSchemaProp{
			Type:        f.Type,
			Enum:        f.Enum,
			Format:      f.Format,
			Unit:        f.Unit,
			Semantics:   f.Semantics,
			Consistency: f.Consistency,
			Sensitivity: f.Sensitivity,
			FactVersion: f.Version,
			OnMissing:   f.OnMissing,
			Nullable:    f.Nullable,
			HasDefault:  f.HasDefault,
			Default:     f.Default,
			Freshness:   f.Freshness,
			Deprecated:  f.Deprecated,
			ReplacedBy:  f.ReplacedBy,
			Source:      f.Source,
		}
		if f.Required {
			doc.Required = append(doc.Required, f.Fact)
		}
	}
	sort.Strings(doc.Required)
	return doc
}

type slotTableDoc struct {
	Scope          string              `json:"scope"`
	CatalogVersion string              `json:"catalogVersion"`
	Slots          []contracts.SlotDef `json:"slots"`
}

// buildSlotTable 按事实名升序生成目录级 slot 表（确定性：同一份目录必得同一组编号）。
func buildSlotTable(list []contracts.FactSchema) []contracts.SlotDef {
	slots := make([]contracts.SlotDef, 0, len(list))
	for _, f := range list {
		slots = append(slots, contracts.SlotDef{Name: f.Fact, Type: f.Type, FactVersion: f.Version})
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	return slots
}
