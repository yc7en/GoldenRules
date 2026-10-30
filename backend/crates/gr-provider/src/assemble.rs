//! Assemble assembler: business inputs -> call Providers -> `FactSnapshot` (entry layer, never enters the execution kernel).
//!
//! ## Boundary (`ARCHITECTURE.md` §2.2 / A-2)
//!
//! The execution kernel only accepts an "assembled snapshot". Fetching, assembling and degradation bookkeeping all happen in this layer;
//! the assembler **does not judge business** and **does not decide** -- whatever goes wrong is expressed as
//! `missingFacts` / `degradedFacts` in the snapshot, and closed out by B4's `onMissing` / `onStale` strategies.
//!
//! ## Three assembly rules (this is where determinism comes from)
//!
//! 1. **Inputs win**: facts passed explicitly by the caller are not overwritten by Providers (inputs are "values I already know").
//! 2. **Declaration order is execution order**: Providers run in config order, and a derived Provider only sees earlier results.
//! 3. **Two Providers in the same scene may not declare the same fact**: rejected at load time, with no implicit runtime overwrite.
//!
//! ## `null` in inputs
//!
//! Always treated as "not provided" (lands in `missingFacts` + `NOT_PROVIDED`) and never enters `facts`.
//! Reason: `contracts/02` §3.2 explicitly forbids expressing absence with `null`, and a `null` in the snapshot would make
//! "not fetched" and "fetched an empty value" indistinguishable, which breaks replay analysis.

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

/// Assembler version, written into the snapshot's `assemblerVersion` (for troubleshooting, not used in freshness checks).
pub const ASSEMBLER_VERSION: &str = "gr-provider/0.1";

const DEFAULT_TIMEOUT_MS: u64 = 50;

pub struct Assembler {
    catalog: Arc<FactCatalog>,
    /// Scene -> Provider chain (the Vec order is the execution order)
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

    /// Build from the declarative config, with **all validation done at load time** (catalog exists / no duplicate supply / dependencies declared).
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

    /// Configured scenes and Provider names (for health checks / self-test)
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

    /// Assemble-mode entry point: assemble the snapshot. The returned snapshot can be fed straight into `Engine::decide`.
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

        // (1) Inputs land first: explicit values win, and this defines the semantics of null
        for (fact, raw) in &inputs {
            if raw.is_null() {
                missing.insert(fact.clone(), self.missing_fact(fact, MissingReason::NotProvided));
            } else {
                facts.insert(fact.clone(), raw.clone());
            }
        }

        // (2) Providers are applied in declaration order (a derivation only sees earlier results)
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
        // assembledAt uses the caller's time baseline instead of the system clock: replay and tests must be reproducible (A-5).
        snapshot.assembled_at = Some(ctx.request_time.clone());
        snapshot.assembler_version = Some(self.version.clone());
        Ok(snapshot)
    }

    /// Merge one sample. **First one wins** (inputs > earlier Providers).
    ///
    /// "Two Providers fighting over the same fact" within a scene is already rejected at load time, so here we only handle
    /// the "inputs vs Provider" race, which keeps the rule simple enough to explain.
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
                // A degraded fact still carries its value into the snapshot (`contracts/02` §3.2: degraded = value in facts + registered in degradedFacts)
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
            // Requiredness comes from the fact catalog: a Provider must not decide for itself whether "missing" matters
            required: self.catalog.get(fact).map(|s| s.required).unwrap_or(false),
            fallback: None,
        }
    }

    /// `fs-<timestamp>-<seq>`: the time comes from `requestTime` (reproducible), and the seq keeps it unique within one second.
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

        // (1) Catalog validation: a Provider supplying a fact absent from the catalog = config bug, reported at load time
        for fact in spec.declared_facts() {
            if catalog.get(&fact).is_none() {
                return Err(ErrorContract::new(
                    ErrorCode::IrUnknownFact,
                    format!("场景 {} 的 Provider {name} 声明了目录中不存在的事实 {fact}", scene.scene),
                )
                .with_detail("scene", serde_json::json!(scene.scene))
                .with_detail("provider", serde_json::json!(name)));
            }
            // (2) Duplicate supply: two Providers in the same scene may not fight over the same fact
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
                    // A duplicate declaration of the same name resolves to the same slot index, which is a misconfiguration
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
    // Config errors reuse IR_INVALID: it is the semantic slot for "load-time structural error";
    // we do not mint a new error code because the error code table is part of the contract (`contracts/05` §1), and adding one means syncing the governance side.
    ErrorContract::new(ErrorCode::IrInvalid, msg)
}
