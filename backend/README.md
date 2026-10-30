# GoldenRules-backend

GoldenRules 的**后端**（引擎计算 + BRMS 操作）。前端在 `../GoldenRules`（React）。
治理规则与契约在 `../docs-local/GoldenRules/`，**代码必须与契约一致**：

- 分层与不变量：`../docs-local/GoldenRules/ARCHITECTURE.md`
- 字段级契约：`../docs-local/GoldenRules/contracts/01..06`
- 工作包与验收：`../docs-local/GoldenRules/PLAN.md`（B0–B12）

## 结构

```
Cargo.toml                  # Rust workspace（引擎侧）
crates/
  gr-contract/              # B0 共享契约类型（IR/规则包/快照/DecisionResult/ErrorContract）
  gr-ir/                    # B1 IR 加载、slot 表、按 scene 剪枝
  gr-vm/                    # B2 IR→字节码编译 + VM + 命中策略 + decimal
  gr-engine/                # B3 决策入口 + DecisionResult + 决策日志；B4 错误与降级
  gr-provider/              # B5 内置 Provider + Assemble 组装
  gr-server/                # 决策服务进程（HTTP/JSON）
platform/                   # Go 管理平台（B0 只有骨架 + 契约绑定）
fixtures/                   # 契约样例，两语言共读，跨语言一致性的唯一裁判
```

## 两条分工红线

1. **决策服务只做「加载规则包 → 执行 → 决策日志」**：不取数、不调外部服务、不做业务动作（A-2）。
   取数与组装只在 Assemble 入口层，且不进执行内核。
2. **管理平台产出规则包，不依赖决策服务运行时状态**（A-1）；两者只通过「规则包产物 + 契约」对接。

## 构建与测试

```bash
cargo test                     # Rust 引擎侧全部单测
cargo test --release -- --nocapture bench   # 性能基线（千条规则）
cd platform && go test ./...   # Go 契约绑定往返测试
cd platform && go run ./cmd/platformd -addr :8080
```

契约一致性：Rust 的 `crates/gr-contract/tests/roundtrip.rs` 与 Go 的
`platform/contracts/roundtrip_test.go` 读**同一份** `fixtures/`，两侧都必须绿。
