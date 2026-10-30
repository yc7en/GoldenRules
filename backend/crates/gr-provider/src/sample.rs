//! Provider contract: the read-only input view for fetching + the per-fact result + the `Provider` trait.
//!
//! ## Why `fetch` does not return `Result`
//!
//! In the contract, a fetch failure **is not an error but a fact state** (`contracts/02` §3.2):
//! a timeout must land in `missingFacts` + `PROVIDER_TIMEOUT`, and the `onMissing` strategy decides the consequence
//! (`ERROR` -> REJECT / `DEFAULT` -> fallback / `SKIP` -> skip the rule / `FALLBACK_DECISION`).
//! If `fetch` could return `Err`, callers would think "the whole request failed", which is exactly the bypass of
//! fail-closed's tiered degradation -- so failures are forced into `FactSample::Missing`.
//!
//! Only configuration errors of the **assembler itself** (scene does not exist, a Provider claims a fact that is not in the catalog)
//! are `ErrorContract`, because those are load-time bugs, not runtime fact states.

use gr_contract::{DegradeReason, MissingReason};
use std::collections::BTreeMap;

pub type JsonMap = BTreeMap<String, serde_json::Value>;
pub type FactMeta = BTreeMap<String, serde_json::Value>;

/// Read-only view visible to one fetch.
#[derive(Clone, Copy, Debug)]
pub struct ProviderCtx<'a> {
    pub tenant_id: &'a str,
    pub scene: &'a str,
    /// The single time baseline, taken from `ExecutionContext.request_time` (the engine does not read the system clock, A-5).
    /// Providers needing a window such as "the last 24 hours" may only compute it against this time.
    pub request_time: &'a str,
    /// Caller business inputs (the raw input of Assemble mode).
    pub inputs: &'a JsonMap,
    /// Facts **already assembled** so far. Derived Providers depend on it, so Provider order is part of the semantics.
    pub facts: &'a JsonMap,
}

/// Supply result for a single fact (three states, in one-to-one correspondence with `contracts/02` §3.2).
#[derive(Clone, Debug, PartialEq)]
pub enum FactSample {
    /// Supplied normally. `meta` carries `eventTime` / `sourceTimestamp` etc. (the raw material for freshness checks).
    Value { value: serde_json::Value, meta: Option<FactMeta> },
    /// Not fetched. Lands in `missingFacts`.
    Missing(MissingReason),
    /// Fetched but untrustworthy (expired, partially degraded upstream). The value still goes into `facts`, and lands in `degradedFacts` as well.
    Degraded {
        value: serde_json::Value,
        reason: DegradeReason,
        fallback: Option<String>,
        meta: Option<FactMeta>,
    },
}

impl FactSample {
    pub fn value(value: serde_json::Value) -> Self {
        FactSample::Value { value, meta: None }
    }

    /// Attach metadata (e.g. `eventTime`). An undeclared meta key warns but is not rejected (`contracts/02` §4).
    pub fn with_meta(mut self, key: impl Into<String>, value: serde_json::Value) -> Self {
        let key = key.into();
        match &mut self {
            FactSample::Value { meta, .. } | FactSample::Degraded { meta, .. } => {
                meta.get_or_insert_with(BTreeMap::new).insert(key, value);
            }
            FactSample::Missing(_) => {}
        }
        self
    }

    pub fn missing(reason: MissingReason) -> Self {
        FactSample::Missing(reason)
    }

    pub fn degraded(value: serde_json::Value, reason: DegradeReason) -> Self {
        FactSample::Degraded { value, reason, fallback: None, meta: None }
    }
}

/// A fetch unit. Implementors only answer "what is the current value of the facts I own"; they never decide and never touch the snapshot.
pub trait Provider: Send + Sync {
    fn name(&self) -> &str;

    /// The fact names this Provider owns. The assembler uses it at load time for catalog validation and duplicate-supply detection.
    fn provides(&self) -> &[String];

    /// Fetch. The returned order is the assembly order (for the same fact, first one wins).
    fn fetch(&self, ctx: &ProviderCtx<'_>) -> Vec<(String, FactSample)>;
}
