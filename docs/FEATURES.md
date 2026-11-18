---
version: v0.1.0
source: GoldenRules@8c1f061
generated: 2026-11-18
lang: en
---

# GoldenRules Feature Inventory

This is the external release snapshot of the feature inventory. Each entry describes current code behavior in one sentence and carries a stable ID that does not change or get reused. This snapshot lists only implemented features; planning and in-progress items are intentionally omitted.

Status legend: **Implemented** — code complete and passing type checks, unit tests, and build.

## 1 Engine execution · F-ENG

| ID | Feature | Description | Status |
|---|---|---|---|
| F-ENG01 | Shared contract types | `RulePackage` / `RuleSet` / `Rule` / `Expr` IR / `FactCatalog` / `FactSnapshot` / `DecisionResult` / `ErrorContract` have Rust and Go bindings that round-trip identically against the same fixtures | Implemented |
| F-ENG02 | Exact decimal arithmetic | Amounts and rates use decimal values serialized as strings in JSON; no floating point is involved | Implemented |
| F-ENG03 | RFC3339 time parsing | A minimal built-in parser (UTC / offset / milliseconds) supports fact-freshness calculation | Implemented |

## 2 Operations console (BRMS UI) · F-FE

All pages read from platform APIs. Trial runs are forwarded to the engine by the platform. The console does not implement its own evaluator or validator.

| ID | Feature | Description | Status |
|---|---|---|---|
| F-FE01 | Scene strategy page | Scene list plus an ordered list of attached rulesets (not a node canvas); grouped by ruleset scene with the engine's loaded packages shown | Implemented |
| F-FE02 | Ruleset editor | Views are dispatched by storage form: `rules[]` gets structured / expression / decision tree, `table` gets structured / decision table, `crossTable` gets structured / cross table. All share one DSL/IR; saves run server-side dry-run validation | Implemented |
| F-FE03 | Dry-run validation | Calls `POST /rulesets/{id}/validate`: the real DSL compiler and fact catalog return error codes, paths, and suggestions | Implemented |
| F-FE04 | Trial run | Injects facts to produce a decision and hit path; forwarded through the platform to `/v1/trial`; the UI states that the loaded package, not the draft, is executed | Implemented |
| F-FE05 | Governance pages | Fact catalog (read-only), test cases (confirm/reject persisted), approval and release (approve/reject/publish), observability and audit (log replay) | Implemented |
| F-FE06 | Decision tree canvas | A ruleset view: conditions level by level with yes/no branches and leaf decisions; the tree compiles into `rules[]` with `FIRST` hit strategy without engine changes. `ALL` rulesets get a read-only star diagram | Implemented |
| F-FE07 | Six-locale UI | `en` default plus `zh-CN` / `ja` / `ar` / `fr` / `de`, with RTL support and locale-aware plurals. Contract enums are not translated, only labeled. Amounts and log timestamps do not change with locale | Implemented |
| F-FE08 | Decision table and cross-table editors | A normal table produces conclusions from row conditions with optional per-row `assign` outputs; a cross table produces a matrix from two axes where each non-empty cell is one rule. Both persist only their own storage form and expand at compile time | Implemented |

## 3 Development and integration tools · F-DEV

| ID | Feature | Description | Status |
|---|---|---|---|
| F-DEV01 | Local end-to-end console | `tools/console/index.html`: a ten-step run from fact catalog through decision replay, showing HTTP status and key fields at each step; "ran but verified nothing" is marked separately from a pass | Implemented |
| F-DEV02 | One-command local environment | `tools/dev-up.sh`: build, start engine and platform, wait for health, print addresses, and clean up by PID | Implemented |
| F-DEV03 | Cross-origin switch | `platformd --cors-origin`, off by default; only self-describing operator headers are allowed, and OPTIONS preflight returns 204 directly | Implemented |

## 4 Engine, decision, and governance features

| ID | Feature | Description | Status |
|---|---|---|---|
| F-ENG04 | IR loading and slot table | Field references resolve to slot indices at load time with bounds/count/depth limits | Implemented |
| F-ENG05 | Scene and prerequisite pruning | Rulesets selected by scene; rules gated by prerequisite facts without reordering | Implemented |
| F-ENG06 | IR-to-bytecode compilation and VM | Flat bytecode with a stack VM and exact decimal operations | Implemented |
| F-ENG07 | Hit strategies | `FIRST`, `ALL`, and `PRIORITY` with stable tie-breaking | Implemented |
| F-ENG08 | Whitelist function library | Only approved operators and functions compile; unknown names are rejected | Implemented |
| F-DEC01 | Snapshot entry and `DecisionResult` | Snapshot-mode decision entry with a structured result and hit rules | Implemented |
| F-DEC02 | `assign` outputs and merging | Output variables merge by priority, reason code, and rule ID; first writer wins | Implemented |
| F-DEC03 | Decision logging | Logs carry all required fields with PII masking by sensitivity level | Implemented |
| F-ERR01 | Contract violations as REJECT | Violations return a `REJECT` decision with `CONTRACT_VIOLATION` and HTTP 200 | Implemented |
| F-ERR02 | Missing-value strategies | Four `onMissing` modes: `ERROR` / `DEFAULT` / `SKIP` / `FALLBACK_DECISION` | Implemented |
| F-ERR03 | Exit validation and degradation matrix | Cross-checks between decision output, hit rules, evaluation counts, and scene output schema | Implemented |
| F-PRV01 | Built-in Providers | Constant, derived, and HTTP Providers, all using the same compiler and budget as rules | Implemented |
| F-PRV02 | Assemble entry | Combines inputs and Providers into a snapshot; missing facts degrade through one exit path | Implemented |
| F-FAC01 | Fact catalog CRUD and idempotent generated artifacts | The catalog is the single source of truth; generated files are compared byte for byte | Implemented |
| F-FAC02 | Artifact staleness check | A CI gate rejects a catalog whose generated artifacts differ from a fresh regeneration | Implemented |
| F-FAC03 | Field contract validation | Three-state consistency, enums, and destructive-change detection | Implemented |
| F-DSL01 | Server-side rule editing and dry-run | The same AST compiles to the same IR across all authoring forms | Implemented |
| F-DSL02 | Four authoring forms, one IR | Structured JSON, text expression, decision table, and cross table compile to one IR | Implemented |
| F-DSL03 | Ruleset repository and fact reference lookup | Referenced facts cannot be deleted while in use | Implemented |
| F-PKG01 | Package compilation | DSL to IR plus slot table and signature, compiled atomically per package | Implemented |
| F-PKG02 | Immutable versions and signature verification | Versions freeze after leaving draft; signatures cover the whole package | Implemented |
| F-PKG03 | Release state machine and effective time | `validFrom` / `validUntil` scheduling with an immutable release history | Implemented |
| F-PKG04 | Rollback by version switch | Rollback moves to an older version without creating a new version number | Implemented |
| F-PKG05 | Case counts and release warnings | Unconfirmed test cases are counted and surfaced at release time | Implemented |
| F-PKG06 | Approved-case release gate | Only approved cases run; any failure blocks release, with preview and fail-closed behavior | Implemented |
| F-GOV01 | Approval matrix | Submitter type × L1–L4 levels, evaluated server-side | Implemented |
| F-GOV02 | Separation of duties | Submit / approve / publish are mutually exclusive; agents can do none of them | Implemented |
| F-GOV03 | Change-request lifecycle and version promotion | Full approval promotes a version from draft to approved in one transaction | Implemented |
| F-GOV04 | Gray release | Shadow / percentage / conditional modes with level-based limits | Implemented |
| F-GOV05 | Governance audit stream | Submit / approve / reject / publish / rollback / gray changes are all recorded | Implemented |
| F-GOV06 | Emergency path and makeup review | L3 and below may publish directly; L4 requires two-person authorization and a 24-hour makeup review | Implemented |
| F-GOV07 | Approval SLA and escalation | Level-based targets with escalation on overdue requests | Implemented |
| F-OBS01 | Decision log ingestion and masked views | Idempotent ingestion with masked query views | Implemented |
| F-OBS02 | Encrypted raw snapshots and retention | Raw snapshots are encrypted and purged lazily after the retention period | Implemented |
| F-OBS03 | Snapshot replay and comparison | Replay pins the historical package version and compares decisions and outputs | Implemented |
| F-OBS04 | Audit events for logs | Log access and replay are recorded on the same audit timeline | Implemented |
| F-OBS05 | Observability summary | Decision/scene distribution, degradation counts, and average/max latency | Implemented |
| F-OBS06 | Log collector sidecar | Batches engine stdout into the platform with retries and explicit drop counts; never backpressures the engine | Implemented |
| F-OBS07 | Key rotation and pluggable key sources | Old keys are retained, ciphertext carries a key ID, and historical data can be rewrapped | Implemented |
| F-REL01 | Execution resource limits and isolation | Load-time, request-time, and admission gates; overload fails fast rather than queuing | Implemented |
| F-REL02 | Load and reliability baseline | Keep-alive, zero-downtime hot reload, and a deterministic benchmark tool | Implemented |

## Known limitations of the current release

- A newly authored ruleset cannot be trial-run until it has been packaged, released, and loaded by the engine.
- Hot reload does not change the package path; it only re-reads the paths configured at startup.
- The HTTP Provider has no TLS support.
- Gray release settings are produced by the platform but are not yet consumed by the engine.
- Replay requires the historical package version to still be loadable.
- CI idempotency checks are enforced by build tooling, not by an online service.
