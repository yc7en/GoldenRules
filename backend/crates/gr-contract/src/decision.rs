//! Decision contract (`contracts/03` / `contracts/05` §7)

use crate::dsl::Decision;
use crate::value::Value;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

pub const RESULT_VERSION: &str = "v1";
pub const ENGINE_VERSION: &str = concat!("engine-v", env!("CARGO_PKG_VERSION"));

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct HitRule {
    pub rule_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rule_version: Option<String>,
    pub priority: i64,
    pub reason_code: String,
    #[serde(default)]
    pub decision: Option<Decision>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DegradedReason {
    pub fact: String,
    pub reason: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fallback: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub code: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DecisionResult {
    pub result_version: String,
    pub snapshot_id: String,
    pub trace_id: String,
    pub ruleset_id: String,
    pub ruleset_version: String,
    pub decision: Decision,
    pub reason_code: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason_message: Option<String>,
    #[serde(default)]
    pub hit_rules: Vec<HitRule>,
    pub evaluated_rules: usize,
    #[serde(default)]
    pub outputs: BTreeMap<String, Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_schema_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_schema_version: Option<String>,
    #[serde(default)]
    pub degraded: bool,
    #[serde(default)]
    pub degraded_reasons: Vec<DegradedReason>,
    #[serde(default)]
    pub execution_time_ms: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub executed_at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub engine_version: Option<String>,
    /// Rule package version (A-4: logs must carry the rule package version)
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub package_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub package_version: Option<String>,
}

/// Decision log entry (A-4: snapshot (masked) + fact catalog version + rule package version + hit rules + outputs + degraded flag + engine version + latency)
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DecisionLogEntry {
    pub snapshot_id: String,
    pub trace_id: String,
    pub tenant_id: String,
    pub scene: String,
    pub fact_schema_version: String,
    pub package_id: String,
    pub package_version: String,
    pub ruleset_id: String,
    pub ruleset_version: String,
    /// Masked snapshot view (PII masking); the raw snapshot never enters this structure (`contracts/03` §6.2)
    pub masked_facts: BTreeMap<String, serde_json::Value>,
    pub masked_fields: Vec<String>,
    pub hit_rules: Vec<HitRule>,
    pub outputs: BTreeMap<String, Value>,
    pub decision: Decision,
    pub reason_code: String,
    pub degraded: bool,
    pub degraded_reasons: Vec<DegradedReason>,
    pub engine_version: String,
    pub execution_time_ms: f64,
    pub evaluated_rules: usize,
    pub executed_at: String,
}
