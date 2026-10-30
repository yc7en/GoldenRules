// Command platformd 是 BRMS 管理平台（Go）进程。
//
// 职责边界（`ARCHITECTURE.md` §2.1）：平台产出规则包与治理记录，**不依赖决策服务的
// 运行时状态**；决策执行永远在 Rust 决策服务里，本进程不内嵌执行器。
//
// 已实现工作包：
//
//	B6 事实目录（facts）—— CRUD、单一真源、幂等生成物、CI 过期校验
//
// 数据落在 -data 指定的目录（一份 JSON 文档一个文件），P0 不引入数据库。
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
	// 发布门禁执行器（F-PKG06）：只有引擎那台 VM 能证明用例真的会得出那个结论。
	// 没配 --engine-url 且包里有 approved 用例时，发布会被拦下（fail-closed），
	// 而不是「反正验不了就先放行」——那等于把「已验证」变成一句没人核实过的话。
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

	// 事实目录删除前问规则集仓库「这个字段还有人用吗」：注入回调，
	// 让 facts 包不反向依赖 dsl 包（依赖方向单向，A-1）。
	factSvc.WithRefChecker(rulesetSvc.ReferencingFact)

	if *seed != "" {
		if err := seedFacts(factSvc, filepath.Join(*seed, "fact-catalog.json")); err != nil {
			log.Fatalf("导入事实目录失败: %v", err)
		}
		if err := seedRulesets(rulesetSvc, filepath.Join(*seed, "rule-package.json")); err != nil {
			log.Fatalf("导入规则集失败: %v", err)
		}
	}

	// 决策日志分两套存储：logs.json 是脱敏视图，raw.json 是原始快照密文（`contracts/03` §8.2）。
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
		// 没有密钥不是致命错误：降级为「只存脱敏视图」，但要吼一声，别让人以为回放能用。
		log.Printf("警告：%v —— 原始快照不落盘，回放不可用", err)
		box = nil
	} else {
		// 挂上托管密钥源：本地缺某把历史钥时按需向 KMS 取（本轮实现为环境变量源，
		// KMS 实现只需满足 obs.KeyProvider 接口即可替换）。
		box.WithKeyProvider(obs.NewEnvKeyProvider())
	}
	obsSvc := obs.New(logStore, rawStore, box).WithRetention(*retention)
	// 注意：必须显式声明为接口类型再赋值。直接把可能为 nil 的 *HTTPReplayer 塞进
	// Deps.Replayer，会得到一个「非 nil 的接口包着 nil 指针」，handler 里 != nil 判断失效。
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

// seedFacts 只在目录为空时导入，避免重启覆盖人工改动。
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

// seedRulesets 把 fixtures 里的规则包拆成规则集定义导入（仅首启、且仓库为空时）。
//
// fixtures 里的规则集已经带 slotTable（IR 形态的残留），这里**丢掉**它——
// 平台侧的真源是 DSL（字段名），slot 表是编译产物，不能反过来当真源导入。
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
