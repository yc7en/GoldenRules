# GoldenRules BRMS 管理平台（前端）

规则引擎「BRMS 操作部分」的治理平台前端。基于后端契约（`docs-local/GoldenRules/contracts/`）反推而来。

## 技术栈

- **React 19 + TypeScript 7 + Vite 8**
- **Tailwind CSS + shadcn/ui 风格组件**（自建轻量 UI 组件，无 shadcn CLI 依赖）
- **TanStack Table**（决策表网格）
- **Monaco Editor**（表达式编辑）
- **React Router**（HashRouter）

> 决策树可视化（若需）P1 起用 React Flow 自定制，P0 不做。

## 设计红线（对应 `设计教训.md`）

1. **表达式 / 决策表是同一规则集的两种视图**，底层同一 DSL/IR，非独立模块、非连线画布。
2. **场景策略 = 规则集挂载列表**，不是节点拖拽编排。
3. **无 ETL / 取数 / 指标菜单**，只有「事实目录」契约视图。
4. **数据模型唯一真源 = 我们的 DSL/IR**，不引入 JDM/DMN/FEEL。

## 运行

前端不再自带数据，必须先起后端（两个进程：Rust 决策服务 + Go 管理平台）：

```bash
# 终端 1：后端（构建 + 起引擎与平台 + 等健康）
cd ../GoldenRules-backend && tools/dev-up.sh

# 终端 2：前端
npm install
npm run dev      # http://localhost:5173
npm run build    # 构建到 dist/
```

平台地址默认 `http://127.0.0.1:18081`，可用环境变量覆盖：

```bash
VITE_PLATFORM_BASE=http://10.0.0.5:18081 npm run dev
```

**界面只连平台，不直连引擎。** 试跑（`POST /v1/trial`）由平台转发，
因为引擎是不带认证的内部服务，给它开跨源等于把决策能力暴露给任意网页。
平台启动时必须带 `--cors-origin`（`dev-up.sh` 已带 `*`），否则浏览器读不到响应，
症状是侧边栏两个指示灯都红、页面报 `NETWORK_UNREACHABLE`。

## 目录结构

```
src/
├── components/
│   ├── Layout.tsx          # 侧边导航 + 主内容区
│   └── ui/                 # 轻量 UI 组件（button/badge/card/table/tabs/input）
├── pages/
│   ├── Scenes.tsx          # 场景策略
│   ├── RuleSets.tsx        # 规则集列表
│   ├── RuleSetEditor.tsx   # ★ 核心：规则集编辑器（结构化/表达式/决策表三视图 + 校验 + 试跑）
│   ├── Facts.tsx           # 事实目录
│   ├── TestCases.tsx       # 测试用例（业务确认）
│   ├── Approval.tsx        # 审批与发布（二维审批矩阵）
│   └── Observability.tsx   # 观测与审计（决策日志/枚举/理由码）
└── lib/
    ├── api.ts              # 平台调用：基址/错误包封/操作者头/超时
    ├── api-types.ts        # 后端契约的镜像类型（字段对齐 platform/contracts）
    └── useAsync.ts         # 取数三态（加载/错误/数据）+ 手动重试
```

> 已删除 `types.ts` / `mock-data.ts` / `engine.ts` / `validator.ts`：
> 前两个是 mock 原型的残留，后两个是**前端自己实现的求值器与校验器**——
> 与 Rust 引擎是两套实现，同一份规则可能算出不同结论，无法对账。
> 现在校验走平台 dry-run，试跑走引擎，前端不再参与判定。

## 契约对应

| 前端模块 | 契约 |
|---|---|
| 场景策略 / 规则集 | `contracts/01`、`06` |
| 事实目录 | `contracts/02` |
| 测试用例 | `contracts/01` §6.1 |
| 审批与发布 | `contracts/04`、`06` |
| 观测与审计 | `contracts/03`、`05` |

## 接入情况

| 页面 | 数据来源 | 可执行动作 |
|---|---|---|
| 场景策略 | `GET /api/v1/rulesets`（按 scene 分组）+ `GET /api/v1/engine` | 只读 |
| 规则集 | `GET /api/v1/rulesets` | 进入编辑器 |
| 规则集编辑器 | `GET /api/v1/rulesets/{id}` | dry-run 校验、保存草稿（PUT）、试跑（经平台转发引擎） |
| 事实目录 | `GET /api/v1/facts` | 只读（登记走接口，暂无表单） |
| 测试用例 | 规则集内 `tests` | 确认 / 拒绝（整份 PUT 回规则集） |
| 审批与发布 | `/api/v1/changes`、`/approval-matrix` | 批准 / 驳回 / 发布（发布走 `POST .../publish`） |
| 观测与审计 | `/api/v1/logs`、`/logs/stats`、`/audit` | 历史回放（`POST .../replay`） |

写操作都以侧边栏「当前操作者」的身份提交（`X-Actor` / `X-Actor-Type`）。
换操作者是审批链路的必要动作：等级不允许自审时，同一个人批两遍会被平台拒。

**试跑跑的是引擎已装载的包，不是编辑器草稿。** 改完规则要先保存、再走发布流程热重载，
试跑结论才会变——界面上有黄底提示，不要把它当成「我改的规则已经生效」。
