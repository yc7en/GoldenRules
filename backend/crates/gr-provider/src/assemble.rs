//! Assemble 组装器：业务入参 → 调 Provider → `FactSnapshot`（入口层，不进执行内核）。
//!
//! ## 边界（`ARCHITECTURE.md` §2.2 / A-2）
//!
//! 执行内核只认「装好的快照」。取数、拼装、降级登记全在这一层完成；
//! 组装器**不判断业务**，也**不做决策**——出了什么问题，都表达成快照里的
//! `missingFacts` / `degradedFacts`，由 B4 的 `onMissing` / `onStale` 策略收口。
//!
//! ## 三条装配规则（确定性来自这里）
//!
//! 1. **入参优先**：调用方显式传的事实不被 Provider 覆盖（入参是「我知道的值」）。
//! 2. **声明顺序即执行顺序**：Provider 按配置顺序执行，派生 Provider 只能看到更早的结果。
//! 3. **同一场景内不允许两个 Provider 声明同一事实**：装载期直接报错，不做运行期隐式覆盖。
//!
//! ## 入参里的 `null`
//!
//! 一律按「未提供」处理（落 `missingFacts` + `NOT_PROVIDED`），不进 `facts`。
//! 理由：`contracts/02` §3.2 明确禁止用 `null` 表达缺失，且快照里出现 `null` 会让
//! 「没取到」与「取到了空值」无法区分，回放时分不清。

use crate::constant::ConstantProvider;
use crate::derived::DerivedProvider;
use crate::http::{HttpProvider, StdTransport, Transport};
use crate::sample::{FactSample, JsonMap, Provider, ProviderCtx};
use crate::spec::{AssemblerSpec, ProviderSpec, SceneSpec};
use gr_contract::{
    ErrorCode, ErrorContract, ExecutionContext, FactCatalog, FactSnapshot, MissingFact,
    MissingReason,
};
use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

/// 组装器版本，落进快照的 `assemblerVersion`（排障用，不参与时效判定）。
pub const ASSEMBLER_VERSION: &str = "gr-provider/0.1";

const DEFAULT_TIMEOUT_MS: u64 = 50;

pub struct Assembler {
    catalog: Arc<FactCatalog>,
    /// 场景 → Provider 链（Vec 顺序即执行顺序）
    scenes: BTreeMap<String, Vec<Arc<dyn Provider>>>,
    version: String,
    seq: AtomicU64,
}

impl std::fmt::Debug for Assembler {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Assembler")
            .field("scenes", &self.scenes.keys().collect::<Vec<_>>())
            .field("version", &self.version)
            .finish()
    }
}

impl Assembler {
    pub fn new(catalog: FactCatalog, scenes: BTreeMap<String, Vec<Arc<dyn Provider>>>) -> Self {
        Self {
            catalog: Arc::new(catalog),
            scenes,
            version: ASSEMBLER_VERSION.to_string(),
            seq: AtomicU64::new(0),
        }
    }

    /// 从声明式配置构建，**装载期完成全部校验**（目录存在 / 无重复供给 / 依赖已声明）。
    pub fn from_spec(
        catalog: &FactCatalog,
        spec: &AssemblerSpec,
    ) -> Result<Self, ErrorContract> {
        Self::from_spec_with_transport(catalog, spec, Arc::new(StdTransport))
    }

    pub fn from_spec_with_transport(
        catalog: &FactCatalog,
        spec: &AssemblerSpec,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, ErrorContract> {
        let mut scenes: BTreeMap<String, Vec<Arc<dyn Provider>>> = BTreeMap::new();

        for scene in &spec.scenes {
            if scenes.contains_key(&scene.scene) {
                return Err(config_error(format!("场景 {} 重复声明", scene.scene)));
            }
            let chain = build_chain(catalog, scene, &transport)?;
            scenes.insert(scene.scene.clone(), chain);
        }

        Ok(Self {
            catalog: Arc::new(catalog.clone()),
            scenes,
            version: ASSEMBLER_VERSION.to_string(),
            seq: AtomicU64::new(0),
        })
    }

    pub fn catalog(&self) -> &FactCatalog {
        &self.catalog
    }

    /// 已配置的场景与 Provider 名（健康检查 / 自测用）
    pub fn describe(&self) -> Vec<(String, Vec<String>)> {
        self.scenes
            .iter()
            .map(|(scene, chain)| {
                (scene.clone(), chain.iter().map(|p| p.name().to_string()).collect())
            })
            .collect()
    }

    pub fn has_scene(&self, scene: &str) -> bool {
        self.scenes.contains_key(scene)
    }

    /// Assemble 模式入口：组装快照。返回的快照可直接喂 `Engine::decide`。
    pub fn assemble(
        &self,
        ctx: &ExecutionContext,
        inputs: JsonMap,
    ) -> Result<FactSnapshot, ErrorContract> {
        let chain = self.scenes.get(&ctx.scene).ok_or_else(|| {
            ErrorContract::new(
                ErrorCode::AssembleSceneUnknown,
                format!("场景 {} 没有配置 Provider", ctx.scene),
            )
            .with_detail("scene", serde_json::json!(ctx.scene))
            .with_detail("configured", serde_json::json!(self.describe().into_iter().map(|(s, _)| s).collect::<Vec<_>>()))
        })?;

        let mut facts: JsonMap = JsonMap::new();
        let mut meta: BTreeMap<String, BTreeMap<String, serde_json::Value>> = BTreeMap::new();
        let mut missing: BTreeMap<String, MissingFact> = BTreeMap::new();
        let mut degraded: BTreeMap<String, gr_contract::DegradedFact> = BTreeMap::new();

        // ① 入参先落：显式值优先，且定义 null 的语义
        for (fact, raw) in &inputs {
            if raw.is_null() {
                missing.insert(fact.clone(), self.missing_fact(fact, MissingReason::NotProvided));
            } else {
                facts.insert(fact.clone(), raw.clone());
            }
        }

        // ② Provider 按声明顺序装（派生只能看到更早的结果）
        for provider in chain {
            let view = ProviderCtx {
                tenant_id: &ctx.tenant_id,
                scene: &ctx.scene,
                request_time: &ctx.request_time,
                inputs: &inputs,
                facts: &facts,
            };
            for (fact, sample) in provider.fetch(&view) {
                self.merge(fact, sample, &mut facts, &mut meta, &mut missing, &mut degraded);
            }
        }

        let mut snapshot = FactSnapshot::new(
            self.next_snapshot_id(&ctx.request_time),
            ctx.tenant_id.clone(),
            ctx.scene.clone(),
            self.catalog.catalog_version.clone(),
        );
        snapshot.facts = facts;
        snapshot.fact_meta = meta;
        snapshot.missing_facts = missing.into_values().collect();
        snapshot.degraded_facts = degraded.into_values().collect();
        // assembledAt 取调用方时间基准而不是系统时钟：回放与测试要可复现（A-5）。
        snapshot.assembled_at = Some(ctx.request_time.clone());
        snapshot.assembler_version = Some(self.version.clone());
        Ok(snapshot)
    }

    /// 合并一个样本。**先到者胜**（入参 > 更早的 Provider）。
    ///
    /// 同场景内「两个 Provider 抢同一事实」已在装载期拒绝，所以这里只需处理
    /// 「入参 vs Provider」这一种竞争，规则简单到可解释。
    fn merge(
        &self,
        fact: String,
        sample: FactSample,
        facts: &mut JsonMap,
        meta: &mut BTreeMap<String, BTreeMap<String, serde_json::Value>>,
        missing: &mut BTreeMap<String, MissingFact>,
        degraded: &mut BTreeMap<String, gr_contract::DegradedFact>,
    ) {
        if facts.contains_key(&fact) {
            return;
        }
        match sample {
            FactSample::Value { value, meta: sample_meta } => {
                if value.is_null() {
                    missing
                        .entry(fact.clone())
                        .or_insert_with(|| self.missing_fact(&fact, MissingReason::NotProvided));
                    return;
                }
                facts.insert(fact.clone(), value);
                if let Some(m) = sample_meta {
                    meta.insert(fact, m);
                }
            }
            FactSample::Degraded { value, reason, fallback, meta: sample_meta } => {
                // 降级也要带值进快照（`contracts/02` §3.2：降级 = 值在 facts + 登记 degradedFacts）
                if !value.is_null() {
                    facts.insert(fact.clone(), value);
                }
                if let Some(m) = sample_meta {
                    meta.insert(fact.clone(), m);
                }
                degraded.insert(fact.clone(), gr_contract::DegradedFact { fact, reason, fallback });
            }
            FactSample::Missing(reason) => {
                missing.entry(fact.clone()).or_insert_with(|| self.missing_fact(&fact, reason));
            }
        }
    }

    fn missing_fact(&self, fact: &str, reason: MissingReason) -> MissingFact {
        MissingFact {
            fact: fact.to_string(),
            reason,
            // 必填性以事实目录为准：Provider 不该自己决定「缺了要不要紧」
            required: self.catalog.get(fact).map(|s| s.required).unwrap_or(false),
            fallback: None,
        }
    }

    /// `fs-<时间戳>-<序号>`：时间来自 `requestTime`（可复现），序号保证同秒内唯一。
    fn next_snapshot_id(&self, request_time: &str) -> String {
        let stamp: String = request_time.chars().filter(|c| c.is_ascii_alphanumeric()).take(16).collect();
        let seq = self.seq.fetch_add(1, Ordering::Relaxed) + 1;
        format!("fs-{stamp}-{seq:06}")
    }
}

fn build_chain(
    catalog: &FactCatalog,
    scene: &SceneSpec,
    transport: &Arc<dyn Transport>,
) -> Result<Vec<Arc<dyn Provider>>, ErrorContract> {
    let mut chain: Vec<Arc<dyn Provider>> = Vec::with_capacity(scene.providers.len());
    let mut owner: BTreeMap<String, String> = BTreeMap::new();

    for (idx, spec) in scene.providers.iter().enumerate() {
        let default_name = format!("{}#{}", spec.kind(), idx);
        let name = spec.name().unwrap_or(&default_name).to_string();

        // ① 目录校验：Provider 供一个目录里没有的事实 = 配置 bug，装载期就报
        for fact in spec.declared_facts() {
            if catalog.get(&fact).is_none() {
                return Err(ErrorContract::new(
                    ErrorCode::IrUnknownFact,
                    format!("场景 {} 的 Provider {name} 声明了目录中不存在的事实 {fact}", scene.scene),
                )
                .with_detail("scene", serde_json::json!(scene.scene))
                .with_detail("provider", serde_json::json!(name)));
            }
            // ② 重复供给：同场景内不允许两个 Provider 抢同一事实
            if let Some(prev) = owner.insert(fact.clone(), name.clone()) {
                return Err(config_error(format!(
                    "场景 {} 中事实 {fact} 同时由 {prev} 与 {name} 供给（隐式覆盖会掩盖配置错误）",
                    scene.scene
                )));
            }
        }

        let provider: Arc<dyn Provider> = match spec {
            ProviderSpec::Static { facts, .. } => Arc::new(ConstantProvider::new(name, facts.clone())),
            ProviderSpec::Derived { facts, depends_on, .. } => {
                let mut deps = Vec::with_capacity(depends_on.len());
                for fact in depends_on {
                    let schema = catalog.get(fact).ok_or_else(|| {
                        ErrorContract::new(
                            ErrorCode::IrUnknownFact,
                            format!("派生 Provider {name} 的 dependsOn 含目录中不存在的事实 {fact}"),
                        )
                    })?;
                    // 同名重复声明会被解析到同一个 slot 下标，属误配
                    if deps.iter().any(|(f, _): &(String, gr_contract::FactKind)| f == fact) {
                        return Err(config_error(format!(
                            "派生 Provider {name} 的 dependsOn 重复声明 {fact}"
                        )));
                    }
                    deps.push((fact.clone(), schema.kind));
                }
                Arc::new(DerivedProvider::new(name, deps, facts.clone())?)
            }
            ProviderSpec::Http { url, facts, timeout_ms, .. } => Arc::new(HttpProvider::new(
                name,
                url.clone(),
                facts.clone(),
                Arc::clone(transport),
                Duration::from_millis(timeout_ms.unwrap_or(DEFAULT_TIMEOUT_MS)),
            )),
        };
        chain.push(provider);
    }

    Ok(chain)
}

fn config_error(msg: String) -> ErrorContract {
    // 配置错误复用 IR_INVALID：它是「装载期结构错误」这一类的语义位，
    // 不新造错误码是因为错误码表属契约（`contracts/05` §1），加一个要同步治理侧。
    ErrorContract::new(ErrorCode::IrInvalid, msg)
}
