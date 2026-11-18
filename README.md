<div align="center">

# GoldenRules

**规则引擎 BRMS：一条规则从编写、校验、审批、发布到留痕的全链路。** 面向风控、运营等需要「规则可治理、决策可审计」的业务团队，一个仓库装下决策服务、管理平台与运营台前端。

![engine](https://img.shields.io/badge/engine-Rust-orange) ![platform](https://img.shields.io/badge/platform-Go-00ADD8) ![web](https://img.shields.io/badge/web-React-61DAFB) ![stage](https://img.shields.io/badge/stage-pre--1.0-yellow) ![docs](https://img.shields.io/badge/docs-%E4%B8%AD%E6%96%87-gray)

> 无 CI / License / Release 徽章是因为项目尚未配置这些基础设施——与其挂失效徽章，不如留白。

</div>

<!-- 截图位：补上 docs/images/overview.png 后取消注释即可启用
![GoldenRules 运营台：规则集编辑器三视图与试跑结果](docs/images/overview.png)
-->

> 📷 **界面总览截图整理中**（占位：`docs/images/overview.png`）。在此之前可直接打开自测台 `backend/tools/console/index.html`，或按下方步骤启动运营台查看实际效果。

## 目录

- [特性](#特性)
- [快速开始](#快速开始)
- [安装](#安装)
- [用法](#用法)
- [目录结构](#目录结构)
- [架构与分工红线](#架构与分工红线)
- [FAQ](#faq)
- [语言约定](#语言约定)
- [历史说明](#历史说明)
- [贡献](#贡献)
- [许可证](#许可证)
- [联系](#联系)

## 特性

- **一个仓库装整套系统**：决策服务、管理平台、运营台前端同仓维护，跨端改动一次提交，版本对应关系由提交图自带。
- **规则多形态、同一套 IR**：结构化 / 表达式 / 决策表 / 决策树是同一规则集的不同视图，底层共用 DSL 与 IR，决策树可无损编译回 `rules[]`。
- **治理闭环**：草稿 → 校验（dry-run）→ 业务确认用例 → 二维审批矩阵 → 规则包发布 → 灰度 → 决策日志与审计留痕。
- **决策可观测可回放**：决策日志与原文密文分离（AES-256-GCM），支持按日志钉死版本与时刻回放；命中路径（trace）可查。
- **跨语言契约一致**：Rust 与 Go 同读一份 `fixtures/`，双侧往返测试保证同一份规则得到同一结论。
- **界面六语言**：`en`（默认）/ `zh-CN` / `ja` / `ar` / `fr` / `de`，含 RTL 支持；契约枚举值不随界面语言变化。

## 快速开始

**类 Unix / Git Bash**（一键拉起引擎与管理平台，再起前端）：

```bash
cd backend && tools/dev-up.sh
cd ../web && npm install && npm run dev
```

打开运营台 http://localhost:5173 ，自测台见终端打印的 `file://.../tools/console/index.html` 地址。

**Windows**：用 `scripts\build.bat` 编译后，`start-all.bat` 一键拉起三个窗口（引擎 → 平台 → 界面）；停 services 用 `stop-all.bat`。详见[安装](#安装)。

## 安装

### 环境要求

| 依赖 | 最低版本 | 用途 |
|------|----------|------|
| Rust | stable | 决策服务（`backend/crates/gr-*`） |
| Go | 1.24+ | 管理平台（`backend/platform`） |
| Node.js | 22+ | 运营台前端（`web`） |
| Bash | 任意（Windows 用 Git Bash） | 运行 `backend/tools/dev-up.sh` |

> ⚠️ **仓库请放在浅层级目录。** `web/node_modules` 里最深路径已 156 字符，加上 `web/` 前缀是 178；放到更深的盘符层级（如 `D:/work/projects/clients/2026/xxx/`）会撞 Windows `MAX_PATH`，第一个报错通常是 Monaco 的深层包。

### 安装途径

没有发布二进制或包管理器分发，当前唯一途径是**源码构建**：

```bash
# 后端：dev-up.sh 会自动 cargo build --release + go build
cd backend && tools/dev-up.sh

# 前端：npm install 安装依赖后启动
cd web && npm install && npm run dev
```

Windows 用户改用 `scripts/` 下的批处理，参数与 `dev-up.sh` 完全一致：

| 脚本 | 作用 |
|---|---|
| `scripts\build.bat` | 编译 `gr-server`（release）与 `platformd`（输出 `.dev\bin\platformd.exe`） |
| `scripts\start-engine.bat` | 决策服务 `127.0.0.1:18080` |
| `scripts\start-platform.bat` | 管理平台 `127.0.0.1:18081`，数据目录 `.dev\data` |
| `scripts\start-web.bat` | 运营台 `http://localhost:5173`（`--strictPort`：端口被占直接失败，不偷偷换 5174） |
| `scripts\start-all.bat` | 三个窗口依次拉起 |
| `scripts\stop-all.bat` | 按**端口**停而非进程名——同机他人若也跑着 `gr-server`，按名字 `taskkill` 会误伤 |

> **批处理编码约定（别乱改）**：`start-engine` / `start-platform` / `start-web` 是纯 ASCII（输出重定向进按 UTF-8 读的日志）；`build` / `start-all` / `stop-all` 是 GBK 中文（只给人看终端）。中文写成 UTF-8 会让 cmd 按字节错位截断，实测必现。

### 验证安装

```bash
curl http://127.0.0.1:18080/healthz   # 决策服务 → 200
curl http://127.0.0.1:18081/healthz   # 管理平台 → 200
```

两侧都返回 200 才算起全；自测台里把「平台地址」填成 `http://127.0.0.1:18081`，点「一键跑全链路」验证完整链路。

## 用法

### 后端启动参数

`backend/tools/dev-up.sh` 把这些参数固定在一处，避免手抄遗漏导致「看起来像 bug」的错误。可用环境变量覆盖：

| 变量 | 含义 | 默认值 |
|------|------|--------|
| `ENGINE_ADDR` | 决策服务监听地址 | `127.0.0.1:18080` |
| `PLATFORM_ADDR` | 管理平台监听地址 | `127.0.0.1:18081` |
| `RELOAD_TOKEN` | reload / engine 共享令牌 | `devtoken` |
| `DATA_DIR` | 数据与二进制目录 | `${TMPDIR:-/tmp}/gr-dev` |
| `LOG_KEY` | 决策日志加密密钥（32 字节 hex） | 内置开发默认值 |

端口冲突示例：`ENGINE_ADDR=127.0.0.1:19080 PLATFORM_ADDR=127.0.0.1:19081 tools/dev-up.sh`

### 前端配置

| 变量 | 含义 | 默认值 |
|------|------|--------|
| `VITE_PLATFORM_BASE` | 管理平台地址 | `http://127.0.0.1:18081` |

跨机访问示例：`VITE_PLATFORM_BASE=http://10.0.0.5:18081 npm run dev`

### 核心操作流

1. **场景策略**：一个场景挂载多个规则集，列表顺序即执行优先级；不是节点拖拽画布。
2. **规则集编辑器**：选形态（表达式 / 决策表 / 决策树）→ dry-run 校验 → 保存草稿 → 试跑（经平台转发引擎）。
3. **测试用例**：AI 生成默认待确认，业务确认后才是发布门禁。
4. **审批发布**：按二维审批矩阵批准的变更单（`changeId`）才能发布；发布后引擎装载新包。
5. **观测审计**：决策日志、理由码、枚举、命中路径与历史回放。

> **界面只连平台，不直连引擎。** 试跑的 `POST /v1/trial` 由平台转发——引擎是不带认证的内部服务，给它开跨源等于把决策能力暴露给任意网页。平台启动必须带 `--cors-origin`（`dev-up.sh` 已带），否则浏览器读不到响应，症状是侧栏两个指示灯都红、页面报 `NETWORK_UNREACHABLE`。

写操作都以侧边栏「当前操作者」身份提交（`X-Actor` / `X-Actor-Type`）。换操作者是审批链路的必要动作：等级不允许自审时，同一个人批两遍会被平台拒。

### 跑测试

```bash
cd backend && cargo test                      # Rust 引擎侧，109 项
cd backend/platform && go test ./...          # Go 管理平台，111 项
cd web && npx tsc -b && npx vite build        # 前端类型检查与构建（build 含 i18n 校验）
```

## 目录结构

```
backend/
  crates/          Rust 决策服务：gr-contract / gr-ir / gr-vm / gr-engine / gr-provider / gr-server / gr-bench
  platform/        Go 管理平台：cmd / contracts / dsl / facts / gov / obs / packages / persist / httpapi
  fixtures/        跨语言共享契约样例（两语言共读，一致性唯一裁判）
  tools/           dev-up.sh 一键联调脚本 + console 自测台
web/               运营台前端（React 19 + Vite 8 + Tailwind + TanStack Table + Monaco + React Router）
scripts/           Windows 启停批处理
docs/              对外英文文档快照：README.md / ARCHITECTURE.md / FEATURES.md（带版本头，发版时由内部文档生成）
```

内部文档不在本仓库，在统一文档仓库 `docs-local/GoldenRules/`（中文）。

## 架构与分工红线

1. **决策服务只做「加载规则包 → 执行 → 决策日志」**：不取数、不调外部服务、不做业务动作；取数与组装只在 Assemble 入口层，且不进执行内核。
2. **管理平台产出规则包，不依赖决策服务运行时状态**：两者只通过「规则包产物 + 契约」对接。
3. **前端不参与判定**：已删除前端自建的求值器与校验器——那会与 Rust 引擎形成两套实现，同一规则可能算出不同结论、无法对账。校验走平台 dry-run，试跑走引擎。

## FAQ

**Q1：试跑报 `VERSION_RULESET_NOT_FOUND`，或改完规则试跑结果没变？**
A：试跑跑的是**引擎已装载的已发布包**，不是编辑器草稿。要先保存 → 走发布流程让引擎装载新包，结论才会变。界面上有黄底提示；编辑器「校验」不受影响。换包需重启引擎（reload 只重读启动时的路径，不换包）。

**Q2：侧栏两个指示灯都红、页面报 `NETWORK_UNREACHABLE`？**
A：管理平台没带 `--cors-origin`、或引擎/平台进程没起。用 `dev-up.sh` 启动已自动带；确认两个 `/healthz` 都返回 200。

**Q3：发布被门禁拦下 / 报 fail-closed 相关错误？**
A：发布必须挂已批准的变更单（`changeId`）；actor 身份缺省按 agent 处理，也是 fail-closed。检查是否已走审批、操作者身份是否正确。

**Q4：为什么不能自己批准自己的规则？**
A：职责分离——按「提交者类型 × L1~L4」二维审批矩阵判定，等级不允许自审时同一人批两遍会被平台拒。换侧边栏操作者再批。

**Q5：数据怎么重置？Windows 上 `npm install` 撞 `MAX_PATH` 怎么办？**
A：重置数据删 `.dev/data`（批处理）或 `$TMPDIR/gr-dev`（`dev-up.sh`）。`MAX_PATH` 问题把仓库整体移到更浅的目录即可，见[环境要求](#环境要求)。

## 语言约定

| 对象 | 语言 | 说明 |
|---|---|---|
| 代码注释、提交信息、标识符 | **英文** | 注释写「为什么」，不写「是什么」 |
| 控制台输出、脚本提示、README | **中文** | 诊断信息给人看，中文读得快 |
| 界面文案 | 六语言 | `en` / `zh-CN` / `ja` / `ar` / `fr` / `de` |
| 内部文档 | 中文 | `docs-local/GoldenRules/` |
| 对外文档 | 英文 | 本仓库 `docs/`，发版快照 |

**契约枚举不翻译**：`APPROVE` / `REVIEW` / 理由码 / 审计动作的值永远保持英文原样，界面只配显示标签。金额与日志时间也不随界面语言变——阿拉伯语 locale 默认输出东阿拉伯数字和 `٫` 小数点，同一笔钱在两种界面下看起来不同，对账和截图留证会出问题。

语言切换器在侧边栏底部，与「当前操作者」并列。首次访问按 `localStorage["gr.locale"]` → `navigator.languages` → `en` 协商，中文浏览器首次打开是中文。

## 历史说明

`backend/` 由独立仓库 `GoldenRules-backend` 于 2026-10-31 以子树合并方式并入（19 条提交，哈希原样保留）。旧提交记录的是**根级路径**（`crates/…` 而非 `backend/crates/…`），查旧历史要用：

```bash
git log --full-history -- crates/gr-vm
git log --full-history --follow -- <文件>
```

## 贡献

本项目为内部项目，暂无公开的 issue / PR / CONTRIBUTING 流程。改动请在团队内部走评审，并遵守[语言约定](#语言约定)（代码注释与提交信息用英文）。跨端改动尽量放在同一提交，避免引擎与平台版本漂移。

## 许可证

**尚未指定开源许可证，仓库暂无 `LICENSE` 文件。** 当前为内部项目，未对外授权。若需开源，请先补充 `LICENSE` 并在此处同步声明类型与义务（如 MIT 需保留版权声明、Apache-2.0 含专利授权条款、GPL 要求衍生作品同许可证开源）。

## 联系

内部项目，暂无公开讨论区、群或邮箱入口。设计与契约文档在内部仓库 `docs-local/GoldenRules/`（`ARCHITECTURE.md`、`contracts/01..06`、`PLAN.md`）。

---

> 📅 文档最后更新：**2026-11-18**，对应 v0.1 / pre-1.0 开发阶段。界面与接口以代码及内部契约为准。
