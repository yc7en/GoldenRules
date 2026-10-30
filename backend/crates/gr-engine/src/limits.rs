//! B11 · Execution resource caps and isolation (A-9)
//!
//! This is a **safety valve shared by multiple teams**, not a performance optimization: when one team writes a cartesian-product-style mess of rules,
//! the other teams must not pay for it.
//!
//! ## Three gates, three ways to fail
//!
//! | Gate | Timing | Failure shape |
//! |---|---|---|
//! | Load time (rule count / `dependsOn` depth / total instructions / memory estimate) | load | real error, the package never gets in (`EXEC_RULE_LIMIT_EXCEEDED`) |
//! | Request time (snapshot size / instruction budget / scene timeout) | every decision | contract problem -> REJECT decision; **timeout / budget overrun -> real error** |
//! | Admission (in-flight requests) | before entering `decide` | real error `EXEC_OVERLOAD` (503), **no queuing** |
//!
//! ## Why overload must "fail fast" instead of queueing
//!
//! Queueing reshapes "overload" into "unbounded latency": the caller believes the request is still being processed, retries after the timeout,
//! and the load climbs further -- the textbook path to a cascade. Failing fast hands the choice back to the caller (fail-closed, D5),
//! and **overload is a real error rather than a REJECT decision**: REJECT is a business conclusion while overload has nothing to do with business,
//! mixing the two makes the caller record "the system is busy" as "this withdrawal was rejected".
//!
//! ## "Isolation" comes down to two things in code
//!
//! 1. **Budgets are not shared**: one `Budget` per decision, so a request that burns its whole budget cannot swallow anyone else's allowance;
//! 2. **Concurrency does not cannibalize itself**: beyond the global in-flight cap, each scene has its own in-flight cap --
//!    otherwise one team's slow scene (say a 20ms heavy ruleset) could eat all the global concurrency
//!    and squeeze the core scene (5ms) out of its capacity.

use serde::Serialize;
use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

/// Limit overrides for a single scene (falls back to the global default when unset).
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SceneLimit {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub step_budget: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub deadline_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_inflight: Option<usize>,
}

impl SceneLimit {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn with_deadline_ms(mut self, ms: u64) -> Self {
        self.deadline_ms = Some(ms);
        self
    }

    pub fn with_step_budget(mut self, steps: u64) -> Self {
        self.step_budget = Some(steps);
        self
    }

    pub fn with_max_inflight(mut self, n: usize) -> Self {
        self.max_inflight = Some(n);
        self
    }
}

/// Effective limits (the result after applying scene overrides), used for self-description and troubleshooting.
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EffectiveLimits {
    pub scene: String,
    pub step_budget: u64,
    pub deadline_ms: u64,
    pub max_inflight: usize,
}

struct SceneSlot {
    used: AtomicUsize,
    max: usize,
}

impl SceneSlot {
    fn new(max: usize) -> Self {
        Self { used: AtomicUsize::new(0), max }
    }
}

/// Admission gate: global in-flight + per-scene in-flight.
pub struct Gate {
    global: AtomicUsize,
    max_global: usize,
    scenes: BTreeMap<String, SceneSlot>,
    fallback: SceneSlot,
}

impl Gate {
    /// `scenes` is "known scene -> per-scene cap"; `max_global` is the hard cap on the sum over all scenes.
    pub fn new(max_global: usize, scenes: Vec<(String, usize)>) -> Self {
        let scenes = scenes
            .into_iter()
            .map(|(name, max)| (name, SceneSlot::new(max)))
            .collect();
        Self {
            global: AtomicUsize::new(0),
            max_global,
            scenes,
            // Scenes not declared in the config (e.g. a newly added scene with no limits yet) take the fallback and stay capped too
            fallback: SceneSlot::new(usize::MAX),
        }
    }

    fn slot(&self, scene: &str) -> &SceneSlot {
        self.scenes.get(scene).unwrap_or(&self.fallback)
    }

    /// Try to take one slot; returns `None` if none is available (the caller immediately degrades to a 503, without waiting).
    pub fn admit(&self, scene: &str) -> Option<Admission<'_>> {
        let slot = self.slot(scene);

        // Take the global slot first: global is the outermost hard cap, so acquire it before trying the scene, otherwise a full scene would still hold a global slot
        let previous = self.global.fetch_add(1, Ordering::AcqRel);
        if previous >= self.max_global {
            self.global.fetch_sub(1, Ordering::AcqRel);
            return None;
        }

        let scene_prev = slot.used.fetch_add(1, Ordering::AcqRel);
        if scene_prev >= slot.max {
            slot.used.fetch_sub(1, Ordering::AcqRel);
            self.global.fetch_sub(1, Ordering::AcqRel);
            return None;
        }

        Some(Admission { global: &self.global, scene: &slot.used })
    }

    pub fn inflight(&self) -> usize {
        self.global.load(Ordering::Acquire)
    }

    pub fn scene_inflight(&self, scene: &str) -> usize {
        self.slot(scene).used.load(Ordering::Acquire)
    }
}

/// RAII slot: returned automatically when `decide` returns, including the panic-unwind path.
pub struct Admission<'a> {
    global: &'a AtomicUsize,
    scene: &'a AtomicUsize,
}

impl Drop for Admission<'_> {
    fn drop(&mut self) {
        self.scene.fetch_sub(1, Ordering::AcqRel);
        self.global.fetch_sub(1, Ordering::AcqRel);
    }
}

/// Resource profile of one rule package (computed once at load time, no accounting at runtime).
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PackageStats {
    pub package_id: String,
    pub package_version: String,
    pub rulesets: usize,
    pub rules: usize,
    pub instrs: usize,
    /// Memory estimate (instructions + constants + rule metadata), used for the load-time cap check, not an exact RSS
    pub est_bytes: usize,
}

/// Size of the instruction struct (for estimation; identical across platforms because the fields are fixed-size enums and u32)
pub const INSTR_BYTES: usize = 12;
/// Per-rule metadata estimate (rule_id / reason_code / version strings, etc.)
pub const RULE_META_BYTES: usize = 128;

/// Byte estimate for a constant value (decimals/strings by content length, arrays recursively).
pub fn value_bytes(v: &gr_contract::Value) -> usize {
    use gr_contract::Value;
    match v {
        Value::Null => 0,
        Value::Bool(_) => 1,
        Value::Int(_) => 8,
        Value::Dec(d) => 16 + d.to_string().len(),
        Value::Str(s) => 24 + s.len(),
        Value::Arr(items) => 24 + items.iter().map(value_bytes).sum::<usize>(),
    }
}

/// Byte estimate for one chunk.
pub fn chunk_bytes(chunk: &gr_vm::Chunk) -> usize {
    chunk.code.len() * INSTR_BYTES + chunk.consts.iter().map(value_bytes).sum::<usize>()
}

/// Assemble the "used / cap" view for health checks and self-description.
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LimitsReport {
    pub max_snapshot_bytes: usize,
    pub max_rules_per_package: usize,
    pub max_instrs_per_package: usize,
    pub max_package_bytes: usize,
    pub max_inflight: usize,
    pub inflight: usize,
    pub step_budget: u64,
    pub deadline_ms: u64,
    pub scenes: Vec<EffectiveLimits>,
}

/// Convenience: flatten the config into the Gate arguments inside `Engine::new`.
pub fn scene_caps(
    default_cap: usize,
    overrides: &BTreeMap<String, SceneLimit>,
) -> Vec<(String, usize)> {
    overrides
        .iter()
        .map(|(scene, limit)| (scene.clone(), limit.max_inflight.unwrap_or(default_cap)))
        .collect()
}

/// Shared read-only view (held inside `Engine`).
pub type SharedGate = Arc<Gate>;
