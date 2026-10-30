//! Declarative configuration for Providers (the JSON `--providers` points at).
//!
//! Why configuration instead of asking business teams to write Rust: Provider composition is **scene-level**; it is
//! operations-tunable wiring, not compile-time knowledge. The config is fully validated at load time (catalog / duplicates / dependencies), with zero reflection at runtime.

use crate::sample::JsonMap;
use gr_contract::Expr;
use serde::Deserialize;
use std::collections::BTreeMap;

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AssemblerSpec {
    #[serde(default)]
    pub scenes: Vec<SceneSpec>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SceneSpec {
    pub scene: String,
    /// **Declaration order is execution order**: facts a derived Provider depends on must be supplied by an earlier Provider (or by caller inputs).
    #[serde(default)]
    pub providers: Vec<ProviderSpec>,
}

// Note: `rename_all` on an enum only renames **variant names**; fields inside variants need `rename_all_fields`
// (otherwise `dependsOn` / `timeoutMs` are silently ignored -- exactly the trap the first version of this file fell into).
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase", rename_all_fields = "camelCase", tag = "kind")]
pub enum ProviderSpec {
    /// Static constants: configuration-type facts
    Static {
        #[serde(default)]
        name: Option<String>,
        #[serde(default)]
        facts: JsonMap,
    },
    /// Derived expressions: compute new facts from other facts
    Derived {
        #[serde(default)]
        name: Option<String>,
        #[serde(default)]
        facts: BTreeMap<String, Expr>,
        /// Facts the expression may reference; the order is the slot index
        #[serde(default)]
        depends_on: Vec<String>,
    },
    /// HTTP fetch: supplied by an external service
    Http {
        #[serde(default)]
        name: Option<String>,
        url: String,
        /// Fact name -> value path in the response body (`""`/`"$"` = the body itself is an object of fact name -> value)
        #[serde(default)]
        facts: BTreeMap<String, String>,
        #[serde(default)]
        timeout_ms: Option<u64>,
    },
}

impl ProviderSpec {
    pub fn kind(&self) -> &'static str {
        match self {
            ProviderSpec::Static { .. } => "static",
            ProviderSpec::Derived { .. } => "derived",
            ProviderSpec::Http { .. } => "http",
        }
    }

    pub fn name(&self) -> Option<&str> {
        match self {
            ProviderSpec::Static { name, .. }
            | ProviderSpec::Derived { name, .. }
            | ProviderSpec::Http { name, .. } => name.as_deref(),
        }
    }

    /// Fact names declared as supplied (used at load time for catalog validation and duplicate detection).
    pub fn declared_facts(&self) -> Vec<String> {
        match self {
            ProviderSpec::Static { facts, .. } => facts.keys().cloned().collect(),
            ProviderSpec::Derived { facts, .. } => facts.keys().cloned().collect(),
            ProviderSpec::Http { facts, .. } => facts.keys().cloned().collect(),
        }
    }
}
