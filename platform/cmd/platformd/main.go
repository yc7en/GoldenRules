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
	"goldenrules/platform/facts"
	"goldenrules/platform/httpapi"
	"goldenrules/platform/persist"
)

func main() {
	addr := flag.String("addr", ":8080", "管理平台监听地址")
	dataDir := flag.String("data", "./data", "数据目录（JSON 文档存储）")
	artifactsDir := flag.String("artifacts", "./artifacts", "事实目录生成物输出目录")
	seed := flag.String("seed", "", "首启时导入的事实目录样例（fixtures/fact-catalog.json）")
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

	if *seed != "" {
		if err := seedFacts(factSvc, *seed); err != nil {
			log.Fatalf("导入样例失败: %v", err)
		}
	}

	handler := httpapi.New(httpapi.Deps{Facts: factSvc, ArtifactsDir: *artifactsDir})

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
