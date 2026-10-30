// Command platformd is the BRMS management platform (Go) process.
//
// Responsibility boundary (`ARCHITECTURE.md` §2.1): the platform produces rule packages and governance records and **does not depend on the
// runtime state of the decision service**; decision execution always lives in the Rust decision service, and this process embeds no executor.
//
// Work packages implemented:
//
//	B6 fact catalog (facts) — CRUD, single source of truth, idempotent artifacts, CI staleness check
//
// Data lands in the directory given by -data (one JSON document per file); P0 introduces no database.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"goldenrules/platform/contracts"
	"goldenrules/platform/dsl"
	"goldenrules/platform/facts"
	"goldenrules/platform/gov"
	"goldenrules/platform/httpapi"
	"goldenrules/platform/obs"
	"goldenrules/platform/packages"
	"goldenrules/platform/persist"
)

func main() {
	addr := flag.String("addr", ":8080", "管理平台监听地址")
	dataDir := flag.String("data", "./data", "数据目录（JSON 文档存储）")
	artifactsDir := flag.String("artifacts", "./artifacts", "事实目录生成物输出目录")
	seed := flag.String("seed", "", "首启时导入的样例目录（内含 fact-catalog.json 与 rule-package.json）")
	logKey := flag.String("log-key", "", "原始快照加密密钥（32 字节 hex/base64）；不给则读 GOLDENRULES_LOG_KEY，再不给就只存脱敏视图")
	retention := flag.Int("replay-retention-days", obs.DefaultRetentionDays, "原始快照保留天数；<=0 表示不保存原始快照")
	engineURL := flag.String("engine-url", "", "决策服务地址（用于回放/试跑编排与发布门禁，不参与决策路径）；不给则回放与门禁不可用")
	engineToken := flag.String("engine-token", "", "调用决策服务管理端点（/v1/admin/*）的 X-Reload-Token；引擎启用 token 时必须给")
	noVerify := flag.Bool("no-verify-gate", false, "关闭 approved 用例发布门禁（仅限演练环境；生产不许关）")
	corsOrigin := flag.String("cors-origin", "", "允许跨源访问的来源（如 * 或 http://localhost:5173），供本地联调控制台直接调接口；默认不启用")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	factStore, err := persist.Open(filepath.Join(*dataDir, "facts.json"), func() facts.Catalog {
		return facts.Catalog{CatalogVersion: facts.CatalogVersion(time.Now().UTC(), 0)}
	})
	if err != nil {
		log.Fatalf("打开事实目录失败: %v", err)
	}
	factSvc := facts.New(factStore)

	rulesetStore, err := persist.Open(filepath.Join(*dataDir, "rulesets.json"), func() dsl.Repo {
		return dsl.Repo{}
	})
	if err != nil {
		log.Fatalf("打开规则集仓库失败: %v", err)
	}
	rulesetSvc := dsl.NewStore(rulesetStore)

	packageStore, err := persist.Open(filepath.Join(*dataDir, "packages.json"), func() packages.Repo {
		return packages.Repo{Active: map[string]string{}}
	})
	if err != nil {
		log.Fatalf("打开规则包仓库失败: %v", err)
	}
	packageSvc := packages.NewService(packageStore)
	// Publish gate executor (F-PKG06): only the VM inside the engine can prove a test case really reaches that conclusion.
	// With no --engine-url and approved test cases in the package, publish is blocked (fail-closed),
	// rather than "we cannot verify it, so let it through" — which would turn "verified" into a claim nobody checked.
	if *noVerify {
		log.Printf("警告：已关闭 approved 用例发布门禁（--no-verify-gate），仅限演练环境")
	} else if *engineURL != "" {
		packageSvc.WithVerifier(packages.NewHTTPTestCaseVerifier(*engineURL, *engineToken))
	}

	govStore, err := persist.Open(filepath.Join(*dataDir, "gov.json"), func() gov.Doc {
		return gov.Doc{Grays: map[string]gov.Gray{}, Seq: map[string]int{}}
	})
	if err != nil {
		log.Fatalf("打开治理仓库失败: %v", err)
	}
	govSvc := gov.NewService(govStore, packageSvc)

	// Before deleting a fact, ask the ruleset repository "is this field still used": a callback is injected
	// so the facts package does not depend on the dsl package in reverse (dependencies stay one-way, A-1).
	factSvc.WithRefChecker(rulesetSvc.ReferencingFact)

	if *seed != "" {
		if err := seedFacts(factSvc, filepath.Join(*seed, "fact-catalog.json")); err != nil {
			log.Fatalf("导入事实目录失败: %v", err)
		}
		if err := seedRulesets(rulesetSvc, filepath.Join(*seed, "rule-package.json")); err != nil {
			log.Fatalf("导入规则集失败: %v", err)
		}
	}

	// Decision logs use two stores: logs.json holds the masked view, raw.json holds the raw snapshot ciphertext (`contracts/03` §8.2).
	logStore, err := persist.Open(filepath.Join(*dataDir, "logs.json"), func() obs.LogDoc {
		return obs.LogDoc{Seq: map[string]int{}}
	})
	if err != nil {
		log.Fatalf("打开决策日志失败: %v", err)
	}
	rawStore, err := persist.Open(filepath.Join(*dataDir, "raw.json"), func() obs.RawDoc {
		return obs.RawDoc{Blobs: map[string]obs.RawSnapshotRef{}}
	})
	if err != nil {
		log.Fatalf("打开原始快照存储失败: %v", err)
	}
	box, err := obs.NewBoxFromEnv(*logKey)
	if err != nil {
		// A missing key is not fatal: degrade to "store the masked view only", but say it loudly so nobody assumes replay works.
		log.Printf("警告：%v —— 原始快照不落盘，回放不可用", err)
		box = nil
	} else {
		// Attach a managed key source: when a historic key is missing locally, fetch it from the KMS on demand (this round it is an env-var source;
		// a KMS implementation only has to satisfy the obs.KeyProvider interface to be swapped in).
		box.WithKeyProvider(obs.NewEnvKeyProvider())
	}
	obsSvc := obs.New(logStore, rawStore, box).WithRetention(*retention)
	// Note: it must be declared as the interface type explicitly before assignment. Stuffing a possibly-nil *HTTPReplayer straight into
	// Deps.Replayer yields a "non-nil interface wrapping a nil pointer", and the != nil check in the handler stops working.
	var replayer obs.Replayer
	if r := obs.NewHTTPReplayer(*engineURL); r != nil {
		replayer = r
	}

	handler := httpapi.New(httpapi.Deps{
		Facts:        factSvc,
		Rulesets:     rulesetSvc,
		Packages:     packageSvc,
		Gov:          govSvc,
		Obs:          obsSvc,
		Replayer:     replayer,
		ArtifactsDir: *artifactsDir,
		CORSOrigin:   *corsOrigin,
		EngineURL:    *engineURL,
		EngineToken:  *engineToken,
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("goldenrules-platform listening on %s (data=%s)", *addr, *dataDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("platform exit: %v", err)
	}
}

// seedFacts imports only when the catalog is empty, so a restart never overwrites manual edits.
func seedFacts(svc *facts.Service, path string) error {
	if len(svc.Catalog().Facts) > 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cat contracts.FactCatalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return err
	}
	for _, f := range cat.Facts {
		if _, issues, err := svc.Create("seed", f); err != nil {
			return fmt.Errorf("%s: %w (%+v)", f.Fact, err, issues)
		}
	}
	log.Printf("已导入 %d 条事实契约（%s）", len(cat.Facts), cat.CatalogVersion)
	return nil
}

// seedRulesets splits the rule package in fixtures into ruleset definitions and imports them (first start only, and only when the repository is empty).
//
// The rulesets in fixtures already carry a slotTable (a leftover of the IR form); it is **discarded** here —
// the source of truth on the platform side is the DSL (field names), and the slot table is a compile product that must not be imported as truth.
func seedRulesets(store *dsl.Store, path string) error {
	if len(store.List("")) > 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var pkg contracts.RulePackage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return err
	}
	for _, rs := range pkg.Rulesets {
		def := dsl.Definition{
			RulesetID:           rs.RulesetID,
			Scene:               rs.Scene,
			Version:             rs.Version,
			HitStrategy:         rs.HitStrategy,
			DefaultDecision:     rs.DefaultDecision,
			OutputSchemaID:      rs.OutputSchemaID,
			OutputSchemaVersion: rs.OutputSchemaVersion,
			Rules:               rs.Rules,
		}
		for i := range pkg.OutputSchemas {
			s := pkg.OutputSchemas[i]
			if s.OutputSchemaID == rs.OutputSchemaID && s.OutputSchemaVersion == rs.OutputSchemaVersion {
				def.OutputSchema = &s
				break
			}
		}
		for _, tc := range pkg.Tests {
			if tc.Scene == rs.Scene {
				def.Tests = append(def.Tests, tc)
			}
		}
		if _, err := store.Put("seed", def); err != nil {
			return fmt.Errorf("%s: %w", rs.RulesetID, err)
		}
	}
	log.Printf("已导入 %d 个规则集（%s）", len(pkg.Rulesets), pkg.PackageID)
	return nil
}
