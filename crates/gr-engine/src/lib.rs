//! GoldenRules B3：决策入口与 `DecisionResult`。
//!
//! 这一层是「快照 → 决策」的**唯一入口**，做四件事：
//! 1. 选规则包/规则集（多义必须显式指定，不猜）；
//! 2. 快照装 slot（一次）；
//! 3. 调 VM 求值 + 汇总输出变量；
//! 4. 组装 `DecisionResult` 与脱敏决策日志。
//!
//! 不做：取数（Assemble 是 B5 的入口层）、错误降级判定（B4）。执行内核 `gr-vm` 不看快照。

pub mod log;
pub mod outputs;
pub mod slots;

use gr_contract::{
    DecisionLogEntry, DecisionResult, DegradedReason, ErrorCode, ErrorContract, ExecutionContext,
    FactCatalog, FactSnapshot, HitRule, RulePackage, Value, ENGINE_VERSION, RESULT_VERSION,
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
    /// 是否对未知字段严格拒绝（`contracts/02` §3.3 默认严格）
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

    /// 已加载的规则包摘要（健康检查用）
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
        let (pkg, program, rs) = self.resolve(&ctx.scene, ctx)?;

        let fill = slots::fill(&rs, snapshot, &self.catalog);
        let plan = rs.plan(&fill.missing);

        let mut budget = Budget::new(self.config.step_budget);
        let started = Instant::now();
        let outcome = evaluate(&program, &fill.values, &ctx.request_time, &plan, &mut budget)?;
        let outputs = outputs::compute_outputs(
            &program,
            &outcome.hits,
            &fill.values,
            &ctx.request_time,
            &mut budget,
        )?;
        let elapsed_ms = started.elapsed().as_secs_f64() * 1000.0;

        if elapsed_ms > self.config.deadline_ms as f64 {
            return Err(ErrorContract::new(
                ErrorCode::ExecTimeout,
                format!(
                    "规则执行 {elapsed_ms:.3}ms 超过预算 {}ms",
                    self.config.deadline_ms
                ),
            )
            .with_detail("budgetMs", serde_json::json!(self.config.deadline_ms))
            .with_detail("actualMs", serde_json::json!(elapsed_ms)));
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

        // 快照自带的降级事实（供给层已经判定过）如实透传（`contracts/02` §3.2）
        let mut degraded_reasons: Vec<DegradedReason> = snapshot
            .degraded_facts
            .iter()
            .map(|d| DegradedReason {
                fact: d.fact.clone(),
                reason: format!("{:?}", d.reason).to_uppercase(),
                fallback: d.fallback.clone(),
                code: None,
                detail: None,
            })
            .collect();
        degraded_reasons.sort_by(|a, b| (&a.fact, &a.reason).cmp(&(&b.fact, &b.reason)));

        let trace_id = ctx
            .trace_id
            .clone()
            .unwrap_or_else(|| format!("trace-{}", snapshot.snapshot_id));
        let executed_at = ctx.request_time.clone();

        let result = DecisionResult {
            result_version: RESULT_VERSION.to_string(),
            snapshot_id: snapshot.snapshot_id.clone(),
            trace_id: trace_id.clone(),
            ruleset_id: program.ruleset_id.clone(),
            ruleset_version: program.version.clone(),
            decision: outcome.decision.clone(),
            reason_code: outcome.reason_code.clone(),
            reason_message: outcome.reason_message.clone(),
            hit_rules: hit_rules.clone(),
            evaluated_rules: outcome.evaluated,
            outputs: outputs.clone(),
            output_schema_id: program.output_schema_id.clone(),
            output_schema_version: program.output_schema_version.clone(),
            degraded: !degraded_reasons.is_empty(),
            degraded_reasons: degraded_reasons.clone(),
            execution_time_ms: Some(round3(elapsed_ms)),
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
            decision: outcome.decision.clone(),
            reason_code: outcome.reason_code.clone(),
            degraded: result.degraded,
            degraded_reasons,
            engine_version: ENGINE_VERSION.to_string(),
            execution_time_ms: round3(elapsed_ms),
            evaluated_rules: outcome.evaluated,
            executed_at,
        };

        Ok(DecisionOutcome { result, log: entry })
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

/// 便捷：把 outputs 里的值按场景 schema 描述成可读文本（排障/日志）
pub fn describe_outputs(outputs: &BTreeMap<String, Value>) -> String {
    outputs
        .iter()
        .map(|(k, v)| format!("{k}={v}"))
        .collect::<Vec<_>>()
        .join(",")
}
