# GoldenRules

规则引擎 BRMS：一条规则从编写、校验、审批、发布到留痕的全链路。

**一个仓库装整套系统。** 2026-10-31 之前前端与后端是两个仓库，跨端改动（比如给平台加引擎转发端点、同时改前端 `api.ts`）必须拆成两次提交，靠提交信息互指来维持「哪个前端配哪个后端」。合并后这个问题从「靠文档维护」变成「提交图自带」。

## 目录

```
backend/     决策服务（Rust，crates/gr-*）+ 管理平台（Go，platform/）
web/         运营台前端（React 19 + Vite 8 + Tailwind）
docs/        对外文档（英文快照，发版时由内部文档生成；当前未启用）
```

内部文档不在本仓库，在统一文档仓库 `docs-local/GoldenRules/`（中文）。

## 跑起来

```bash
# 1. 后端：构建 + 起决策服务(18080)与管理平台(18081) + 等健康
backend/tools/dev-up.sh

# 2. 前端
cd web && npm install && npm run dev      # http://localhost:5173
```

**界面只连平台，不直连引擎。** 试跑的 `POST /v1/trial` 由平台转发——引擎是不带认证的内部服务，给它开跨源等于把决策能力暴露给任意网页。平台启动必须带 `--cors-origin`（`dev-up.sh` 已带），否则浏览器读不到响应，症状是侧栏两个指示灯都红、页面报 `NETWORK_UNREACHABLE`。

## 验证

```bash
cd backend && cargo test            # 109 项
cd backend/platform && go test ./...  # 111 项
cd web && npx tsc -b && npx vite build
```

自测台：`backend/tools/console/index.html`（浏览器直接打开，十步串跑全链路）。

## 环境要求

- Rust（stable）、Go 1.24+、Node 22+
- **仓库放在浅层级目录。** `web/node_modules` 里最深路径已 156 字符，加上 `web/` 前缀是 178；放到更深的盘符层级（如 `D:/work/projects/clients/2026/xxx/`）会撞 Windows `MAX_PATH`，第一个报错的是 Monaco 的深层包。

## 语言约定

| 对象 | 语言 | 说明 |
|---|---|---|
| 代码注释、提交信息、标识符 | **英文** | 注释写「为什么」，不写「是什么」 |
| 控制台输出、脚本提示、README | **中文** | 诊断信息给人看，中文读得快 |
| 界面文案 | 六语言 | `en`（默认）/ `zh-CN` / `ja` / `ar` / `fr` / `de`，见 `docs-local` 的 `界面多语言设计.md` |
| 内部文档 | 中文 | `docs-local/GoldenRules/` |
| 对外文档 | 英文 | 本仓库 `docs/`，发版快照 |

**契约枚举不翻译。** `APPROVE` / `REVIEW` / 理由码 / 审计动作的值永远保持英文原样，界面只给它们配显示标签。金额与日志时间也不随界面语言变——阿拉伯语 locale 默认输出东阿拉伯数字和 `٫` 小数点，同一笔钱在两种界面下看起来不同，对账和截图留证会出问题。

## 历史说明

`backend/` 由独立仓库 `GoldenRules-backend` 于 2026-10-31 以子树合并方式并入（19 条提交，哈希原样保留）。旧提交记录的是**根级路径**（`crates/…` 而非 `backend/crates/…`），所以查旧历史要用：

```bash
git log --full-history -- crates/gr-vm        # 旧路径，需 --full-history
git log --full-history --follow -- <文件>
```
