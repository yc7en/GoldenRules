//! Fact contract (`contracts/02`)
//!
//! The Fact Catalog stores **contracts** only (field name/type/semantics/freshness/masking/version), never **fetch logic**
//! (`ARCHITECTURE.md` §2.2 data supply boundary, `设计教训.md` anti-pattern 3).

use crate::ty::FactKind;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Consistency {
    Strong,
    Eventual,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Sensitivity {
    Pii,
    Internal,
    Public,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum OnMissing {
    /// Land as REJECT (`CONTRACT_*`)
    Error,
    /// Use the default value + degraded
    Default,
    /// Skip the rules that reference this field
    Skip,
    /// Use the scene fallback decision
    #[serde(rename = "FALLBACK_DECISION")]
    FallbackDecision,
}

impl Default for OnMissing {
    fn default() -> Self {
        OnMissing::Error
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum OnStale {
    Degrade,
    Default,
    Error,
}

impl Default for OnStale {
    fn default() -> Self {
        OnStale::Degrade
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Freshness {
    pub max_age_sec: i64,
    #[serde(default)]
    pub on_stale: OnStale,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FactSchema {
    /// `namespace.field`
    pub fact: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
    #[serde(rename = "type")]
    pub kind: FactKind,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub unit: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub semantics: Option<String>,
    #[serde(default, skip_serializing_if = "crate::ty::is_false")]
    pub required: bool,
    #[serde(default, skip_serializing_if = "crate::ty::is_false")]
    pub nullable: bool,
    #[serde(default, skip_serializing_if = "crate::ty::is_false")]
    pub has_default: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub default: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none", rename = "enum")]
    pub enum_values: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub freshness: Option<Freshness>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub consistency: Option<Consistency>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source_version: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub owner: Option<String>,
    #[serde(default)]
    pub sensitivity: Option<Sensitivity>,
    #[serde(default)]
    pub version: Option<String>,
    #[serde(default, skip_serializing_if = "crate::ty::is_false")]
    pub deprecated: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub replaced_by: Option<String>,
    /// Missing-value handling strategy (`contracts/05` §4.1)
    #[serde(default)]
    pub on_missing: OnMissing,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FactCatalog {
    pub catalog_version: String,
    #[serde(default)]
    pub facts: Vec<FactSchema>,
}

impl FactCatalog {
    pub fn index(&self) -> BTreeMap<&str, &FactSchema> {
        self.facts.iter().map(|f| (f.fact.as_str(), f)).collect()
    }

    pub fn get(&self, fact: &str) -> Option<&FactSchema> {
        self.facts.iter().find(|f| f.fact == fact)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum MissingReason {
    NotProvided,
    ProviderTimeout,
    ProviderError,
    Denied,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum DegradeReason {
    Stale,
    ProviderDegraded,
    Partial,
    /// Field unavailable during replay (`contracts/03` §8.3)
    ReplayFieldUnavailable,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MissingFact {
    pub fact: String,
    pub reason: MissingReason,
    #[serde(default)]
    pub required: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fallback: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DegradedFact {
    pub fact: String,
    pub reason: DegradeReason,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fallback: Option<String>,
}

/// Input snapshot (`contracts/02` §3).
///
/// Missing = absent from `facts` + registered in `missingFacts`;
/// degraded = value present in `facts` + registered in `degradedFacts`. Never express missing as `null`.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FactSnapshot {
    pub snapshot_id: String,
    #[serde(default)]
    pub snapshot_version: Option<String>,
    pub tenant_id: String,
    pub scene: String,
    pub fact_schema_version: String,
    #[serde(default)]
    pub facts: BTreeMap<String, serde_json::Value>,
    #[serde(default)]
    pub fact_meta: BTreeMap<String, BTreeMap<String, serde_json::Value>>,
    #[serde(default)]
    pub missing_facts: Vec<MissingFact>,
    #[serde(default)]
    pub degraded_facts: Vec<DegradedFact>,
    #[serde(default)]
    pub assembled_at: Option<String>,
    #[serde(default)]
    pub assembler_version: Option<String>,
}

impl FactSnapshot {
    pub fn new(
        snapshot_id: impl Into<String>,
        tenant_id: impl Into<String>,
        scene: impl Into<String>,
        fact_schema_version: impl Into<String>,
    ) -> Self {
        Self {
            snapshot_id: snapshot_id.into(),
            snapshot_version: Some("v1".to_string()),
            tenant_id: tenant_id.into(),
            scene: scene.into(),
            fact_schema_version: fact_schema_version.into(),
            facts: BTreeMap::new(),
            fact_meta: BTreeMap::new(),
            missing_facts: Vec::new(),
            degraded_facts: Vec::new(),
            assembled_at: None,
            assembler_version: None,
        }
    }
}
