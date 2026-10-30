//! GoldenRules B3+B4：决策入口、`DecisionResult`、决策日志、错误与降级。
//!
//! 这一层是「快照 → 决策」的**唯一入口**，做五件事：
//! 1. 选规则包/规则集（多义必须显式指定，不猜）；
//! 2. 入口校验与缺失策略（契约违规落 REJECT，不落 500）；
//! 3. 快照装 slot（一次）；
//! 4. 调 VM 求值 + 汇总输出变量；
//! 5. 组装 `DecisionResult`、出口校验、脱敏决策日志。
//!
//! 不做：取数（Assemble 是 B5 的入口层）、业务动作。执行内核 `gr-vm` 不看快照。

pub mod guard;
pub mod log;
pub mod outputs;
pub mod slots;

use gr_contract::{
    Decision, DecisionLogEntry, DecisionResult, DegradedReason, ErrorCode, ErrorContract,
    ExecutionContext, FactCatalog, FactSnapshot, HitRule, RulePackage, Value, ENGINE_VERSION,
    RESULT_VERSION,
};
use gr_ir::{LoadLimits, LoadedPackage, LoadedRuleSet};
use gr_vm::{compile_ruleset, evaluate, Budget, RuleSetProgram};
use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Instant;

#[derive(Clone, Debug)]
pub struct EngineConfig {
    /// 单快照大小上限（`contracts/02` §3.4，默认 1 MiB）
    pub max_snapshot_bytes: usize,
    /// 单次决策指令预算（B11 会做成可配；这里先给安全默认值）
    pub step_budget: u64,
    /// 单次执行时间预算（ms，`contracts/01` §4.5 默认 50ms）
    pub deadline_ms: u64,
    /// 未知字段是否严格拒绝（`contracts/02` §3.3；P0 默认严格）
    pub strict_unknown_fields: bool,
}

impl Default for EngineConfig {
    fn default() -> Self {
        Self {
            max_snapshot_bytes: 1024 * 1024,
            step_budget: 200_000,
            deadline_ms: 50,
            strict_unknown_fields: true,
        }
    }
}

/// 加载并编译好的规则包（不可变，可被多线程共享）。
pub struct CompiledPackage {
    pub loaded: Arc<LoadedPackage>,
    pub programs: BTreeMap<String, Arc<RuleSetProgram>>,
}

impl CompiledPackage {
    pub fn build(pkg: &RulePackage, limits: &LoadLimits) -> Result<Self, ErrorContract> {
        let loaded = Arc::new(gr_ir::load(pkg, limits)?);
        let mut programs = BTreeMap::new();
        for (id, rs) in &loaded.rulesets {
            programs.insert(id.clone(), Arc::new(compile_ruleset(rs)?));
        }
        Ok(Self { loaded, programs })
    }
}

#[derive(Clone, Debug)]
pub struct DecisionOutcome {
    pub result: DecisionResult,
    pub log: DecisionLogEntry,
}

pub struct Engine {
    catalog: Arc<FactCatalog>,
    packages: Vec<Arc<CompiledPackage>>,
    config: EngineConfig,
}

impl Engine {
    pub fn new(
        catalog: FactCatalog,
        packages: Vec<RulePackage>,
        config: EngineConfig,
    ) -> Result<Self, ErrorContract> {
        let limits = LoadLimits::default();
        let mut compiled = Vec::with_capacity(packages.len());
        for pkg in &packages {
            compiled.push(Arc::new(CompiledPackage::build(pkg, &limits)?));
        }
        if compiled.is_empty() {
            return Err(ErrorContract::new(
                ErrorCode::VersionPackageNotFound,
                "引擎没有加载任何规则包",
            ));
        }
        Ok(Self { catalog: Arc::new(catalog), packages: compiled, config })
    }

    pub fn config(&self) -> &EngineConfig {
        &self.config
    }

    pub fn catalog(&self) -> &FactCatalog {
        &self.catalog
    }

    pub fn packages(&self) -> &[Arc<CompiledPackage>] {
        &self.packages
    }

    /// 已加载规则包摘要（健康检查用：探「规则包可加载」）
    pub fn describe(&self) -> Vec<(String, String, usize, usize)> {
        self.packages
            .iter()
            .map(|p| {
                (
                    p.loaded.package_id.clone(),
                    p.loaded.package_version.clone(),
                    p.loaded.rulesets.len(),
                    p.loaded.rule_count,
                )
            })
            .collect()
    }

    /// Snapshot 模式决策入口（`contracts/02` §3）。
    pub fn decide(
        &self,
        snapshot: &FactSnapshot,
        ctx: &ExecutionContext,
    ) -> Result<DecisionOutcome, ErrorContract> {
        // --- 版本选择互斥（`contracts/02` §6.2）：这是真错误，不是决策 ---
        if ctx.ruleset_version.is_some() && ctx.gray_tag.is_some() {
            return Err(ErrorContract::new(
                ErrorCode::VersionSelectionConflict,
                "rulesetVersion 与 grayTag 互斥，不可同时指定",
            )
            .with_detail("rulesetVersion", serde_json::json!(ctx.ruleset_version))
            .with_detail("grayTag", serde_json::json!(ctx.gray_tag)));
        }

        let (pkg, program, rs) = self.resolve(&ctx.scene, ctx)?;
        let prepared = guard::prepare(snapshot, ctx, &rs, &self.catalog, &self.config);

        // --- 契约违规 → REJECT 决策（`contracts/05` §1），不落 HTTP 500 ---
        if !prepared.violations.is_empty() {
            return Ok(self.build_outcome(OutcomeParts {
                pkg: &pkg,
                program: &program,
                snapshot,
                ctx,
                decision: Decision::Reject,
                reason_code: guard::CONTRACT_VIOLATION_REASON.to_string(),
                reason_message: Some("请求未通过契约校验".to_string()),
                hit_rules: vec![],
                evaluated: 0,
                outputs: BTreeMap::new(),
                degraded_reasons: prepared.violations,
                elapsed_ms: 0.0,
            }));
        }

        // --- 缺失/降级汇总（供结果与日志使用） ---
        let mut degraded_reasons = snapshot_degraded(snapshot);
        degraded_reasons.extend(guard::defaulted_reasons(&prepared.fill, &rs));

        // --- FALLBACK_DECISION：整个场景走兜底决策 ---
        if let Some(fallback) = prepared.fallback {
            degraded_reasons.push(DegradedReason {
                fact: "_scene".to_string(),
                reason: "FALLBACK_DECISION".to_string(),
                fallback: Some(fallback.to_string()),
                code: Some(ErrorCode::ContractRequiredMissing.as_str().to_string()),
                detail: Some("字段缺失按 onMissing=FALLBACK_DECISION 走场景兜底".to_string()),
            });
            return Ok(self.build_outcome(OutcomeParts {
                pkg: &pkg,
                program: &program,
                snapshot,
                ctx,
                decision: fallback,
                reason_code: "DEGRADE_FALLBACK_DECISION".to_string(),
                reason_message: Some("字段缺失，按场景兜底决策".to_string()),
                hit_rules: vec![],
                evaluated: 0,
                outputs: BTreeMap::new(),
                degraded_reasons,
                elapsed_ms: 0.0,
            }));
        }

        // --- 求值 ---
        let plan = rs.plan_skipping(&prepared.fill.missing, &prepared.skip_slots);
        let mut budget = Budget::new(self.config.step_budget);
        let started = Instant::now();
        let outcome = evaluate(&program, &prepared.fill.values, &ctx.request_time, &plan, &mut budget)?;
        let outputs = outputs::compute_outputs(
            &program,
            &outcome.hits,
            &prepared.fill.values,
            &ctx.request_time,
            &mut budget,
        )?;
        let elapsed_ms = started.elapsed().as_secs_f64() * 1000.0;

        // 服务级超时：真错误（`contracts/05` §4）
        if elapsed_ms > self.config.deadline_ms as f64 {
            return Err(ErrorContract::new(
                ErrorCode::ExecTimeout,
                format!("规则执行 {elapsed_ms:.3}ms 超过预算 {}ms", self.config.deadline_ms),
            )
            .with_detail("budgetMs", serde_json::json!(self.config.deadline_ms))
            .with_detail("actualMs", serde_json::json!(round3(elapsed_ms))));
        }

        let hit_rules: Vec<HitRule> = outputs::hit_order(&program, &outcome.hits)
            .into_iter()
            .map(|idx| {
                let rule = program.program(idx);
                HitRule {
                    rule_id: rule.rule_id.clone(),
                    rule_version: rule.version.clone(),
                    priority: rule.priority,
                    reason_code: rule.reason_code.clone(),
                    decision: Some(rule.decision.clone()),
                }
            })
            .collect();

        degraded_reasons.sort_by(|a, b| (&a.fact, &a.reason).cmp(&(&b.fact, &b.reason)));
        degraded_reasons.dedup_by(|a, b| a.fact == b.fact && a.reason == b.reason);

        // --- 出口校验（`contracts/05` §7）：失败也落决策，不静默 ---
        let schema = program
            .output_schema_id
            .as_ref()
            .zip(program.output_schema_version.as_ref())
            .and_then(|(id, ver)| pkg.loaded.output_schemas.get(&(id.clone(), ver.clone())));
        let hit_ids: Vec<String> = hit_rules.iter().map(|h| h.rule_id.clone()).collect();
        let issues = guard::validate_exit(
            &outcome.decision,
            &outcome.reason_code,
            &hit_ids,
            outcome.evaluated,
            &outputs,
            !degraded_reasons.is_empty(),
            degraded_reasons.len(),
            &program,
            schema,
        );
        if !issues.is_empty() {
            let mut violations: Vec<DegradedReason> = issues
                .into_iter()
                .map(|detail| DegradedReason {
                    fact: "_outputs".to_string(),
                    reason: "CONTRACT_VIOLATION".to_string(),
                    fallback: None,
                    code: Some(ErrorCode::OutputSchemaViolation.as_str().to_string()),
                    detail: Some(detail),
                })
                .collect();
            violations.sort_by(|a, b| a.detail.cmp(&b.detail));
            return Ok(self.build_outcome(OutcomeParts {
                pkg: &pkg,
                program: &program,
                snapshot,
                ctx,
                decision: Decision::Reject,
                reason_code: guard::CONTRACT_VIOLATION_REASON.to_string(),
                reason_message: Some("决策输出未通过出口校验".to_string()),
                hit_rules: vec![],
                evaluated: outcome.evaluated,
                outputs: BTreeMap::new(),
                degraded_reasons: violations,
                elapsed_ms,
            }));
        }

        Ok(self.build_outcome(OutcomeParts {
            pkg: &pkg,
            program: &program,
            snapshot,
            ctx,
            decision: outcome.decision,
            reason_code: outcome.reason_code,
            reason_message: outcome.reason_message,
            hit_rules,
            evaluated: outcome.evaluated,
            outputs,
            degraded_reasons,
            elapsed_ms,
        }))
    }

    /// 组装 `DecisionOutcome`（结果 + 脱敏日志）。所有出口共用一份，避免字段漂移。
    fn build_outcome(&self, parts: OutcomeParts<'_>) -> DecisionOutcome {
        let OutcomeParts {
            pkg,
            program,
            snapshot,
            ctx,
            decision,
            reason_code,
            reason_message,
            hit_rules,
            evaluated,
            outputs,
            degraded_reasons,
            elapsed_ms,
        } = parts;

        let trace_id = ctx
            .trace_id
            .clone()
            .unwrap_or_else(|| format!("trace-{}", snapshot.snapshot_id));
        let executed_at = ctx.request_time.clone();
        let execution_time_ms = round3(elapsed_ms);

        let result = DecisionResult {
            result_version: RESULT_VERSION.to_string(),
            snapshot_id: snapshot.snapshot_id.clone(),
            trace_id: trace_id.clone(),
            ruleset_id: program.ruleset_id.clone(),
            ruleset_version: program.version.clone(),
            decision: decision.clone(),
            reason_code: reason_code.clone(),
            reason_message: reason_message.clone(),
            hit_rules: hit_rules.clone(),
            evaluated_rules: evaluated,
            outputs: outputs.clone(),
            output_schema_id: program.output_schema_id.clone(),
            output_schema_version: program.output_schema_version.clone(),
            degraded: !degraded_reasons.is_empty(),
            degraded_reasons: degraded_reasons.clone(),
            execution_time_ms: Some(execution_time_ms),
            executed_at: Some(executed_at.clone()),
            engine_version: Some(ENGINE_VERSION.to_string()),
            package_id: Some(pkg.loaded.package_id.clone()),
            package_version: Some(pkg.loaded.package_version.clone()),
        };

        let (masked_facts, masked_fields) = log::mask_snapshot(snapshot, &self.catalog);
        let entry = DecisionLogEntry {
            snapshot_id: snapshot.snapshot_id.clone(),
            trace_id,
            tenant_id: snapshot.tenant_id.clone(),
            scene: snapshot.scene.clone(),
            fact_schema_version: snapshot.fact_schema_version.clone(),
            package_id: pkg.loaded.package_id.clone(),
            package_version: pkg.loaded.package_version.clone(),
            ruleset_id: program.ruleset_id.clone(),
            ruleset_version: program.version.clone(),
            masked_facts,
            masked_fields,
            hit_rules,
            outputs,
            decision,
            reason_code,
            degraded: result.degraded,
            degraded_reasons,
            engine_version: ENGINE_VERSION.to_string(),
            execution_time_ms,
            evaluated_rules: evaluated,
            executed_at,
        };

        DecisionOutcome { result, log: entry }
    }

    /// 选包 + 选规则集。歧义一律报错，不做「猜一个」。
    fn resolve(
        &self,
        scene: &str,
        ctx: &ExecutionContext,
    ) -> Result<(Arc<CompiledPackage>, Arc<RuleSetProgram>, Arc<LoadedRuleSet>), ErrorContract> {
        let mut candidates: Vec<&Arc<CompiledPackage>> = self
            .packages
            .iter()
            .filter(|p| p.loaded.scene_index.contains_key(scene))
            .collect();

        if let Some(version) = &ctx.package_version {
            candidates.retain(|p| &p.loaded.package_version == version);
        }
        if candidates.is_empty() {
            return Err(ErrorContract::new(
                ErrorCode::VersionPackageNotFound,
                format!("没有包含场景 {scene} 的规则包（packageVersion={:?}）", ctx.package_version),
            ));
        }

        let mut matched: Vec<(&Arc<CompiledPackage>, Arc<LoadedRuleSet>)> = Vec::new();
        let mut first_err: Option<ErrorContract> = None;
        for pkg in candidates {
            match pkg.loaded.select_ruleset(
                scene,
                ctx.ruleset_id.as_deref(),
                ctx.ruleset_version.as_deref(),
            ) {
                Ok(rs) => matched.push((pkg, rs.clone())),
                Err(e) => {
                    if first_err.is_none() {
                        first_err = Some(e.into());
                    }
                }
            }
        }

        match matched.len() {
            1 => {
                let (pkg, rs) = matched.pop().expect("len checked");
                let program = pkg.programs.get(&rs.ruleset_id).cloned().ok_or_else(|| {
                    ErrorContract::new(
                        ErrorCode::ExecInternal,
                        format!("规则集 {} 未编译", rs.ruleset_id),
                    )
                })?;
                Ok((pkg.clone(), program, rs))
            }
            0 => Err(first_err.unwrap_or_else(|| {
                ErrorContract::new(
                    ErrorCode::VersionRulesetNotFound,
                    format!("场景 {scene} 下没有可用规则集"),
                )
            })),
            _ => Err(ErrorContract::new(
                ErrorCode::VersionSelectionConflict,
                format!(
                    "场景 {scene} 命中 {} 个规则集，请显式指定 rulesetId / packageVersion",
                    matched.len()
                ),
            )),
        }
    }
}

struct OutcomeParts<'a> {
    pkg: &'a CompiledPackage,
    program: &'a RuleSetProgram,
    snapshot: &'a FactSnapshot,
    ctx: &'a ExecutionContext,
    decision: Decision,
    reason_code: String,
    reason_message: Option<String>,
    hit_rules: Vec<HitRule>,
    evaluated: usize,
    outputs: BTreeMap<String, Value>,
    degraded_reasons: Vec<DegradedReason>,
    elapsed_ms: f64,
}

fn snapshot_degraded(snapshot: &FactSnapshot) -> Vec<DegradedReason> {
    snapshot
        .degraded_facts
        .iter()
        .map(|d| DegradedReason {
            fact: d.fact.clone(),
            reason: format!("{:?}", d.reason).to_uppercase(),
            fallback: d.fallback.clone(),
            code: None,
            detail: None,
        })
        .collect()
}

fn round3(v: f64) -> f64 {
    (v * 1000.0).round() / 1000.0
}

/// 便捷：从 JSON 文本装配引擎（服务进程用）
pub fn engine_from_json(
    catalog_json: &str,
    packages_json: &[String],
    config: EngineConfig,
) -> Result<Engine, ErrorContract> {
    let catalog: FactCatalog = serde_json::from_str(catalog_json).map_err(|e| {
        ErrorContract::new(ErrorCode::IrInvalid, format!("事实目录 JSON 解析失败: {e}"))
    })?;
    let mut packages = Vec::with_capacity(packages_json.len());
    for raw in packages_json {
        let pkg: RulePackage = serde_json::from_str(raw).map_err(|e| {
            ErrorContract::new(ErrorCode::IrInvalid, format!("规则包 JSON 解析失败: {e}"))
        })?;
        packages.push(pkg);
    }
    Engine::new(catalog, packages, config)
}
