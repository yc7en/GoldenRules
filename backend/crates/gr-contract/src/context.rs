//! Execution context (`contracts/02` §5 / §6.2)
//!
//! `requestTime` is the single time baseline (A-5): the engine never reads the system clock, so replay and tests stay reproducible.

use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ExecutionContext {
    pub tenant_id: String,
    pub scene: String,
    /// RFC3339 (UTC), the single time baseline
    pub request_time: String,
    /// Select the ruleset only (must be given explicitly when a scene has several rulesets attached; P0 does not orchestrate multiple rulesets)
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ruleset_id: Option<String>,
    /// Select the ruleset version only (cannot be passed together with grayTag, `contracts/02` §6.2)
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ruleset_version: Option<String>,
    /// Canary tag (mutually exclusive with rulesetVersion)
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gray_tag: Option<String>,
    /// Pin the rule package version (default = currently active version)
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub package_version: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub trace_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub idempotency_key: Option<String>,
}

impl ExecutionContext {
    pub fn new(
        tenant_id: impl Into<String>,
        scene: impl Into<String>,
        request_time: impl Into<String>,
    ) -> Self {
        Self {
            tenant_id: tenant_id.into(),
            scene: scene.into(),
            request_time: request_time.into(),
            ruleset_id: None,
            ruleset_version: None,
            gray_tag: None,
            package_version: None,
            trace_id: None,
            idempotency_key: None,
        }
    }

    pub fn request_time_secs(&self) -> Option<i64> {
        crate::time::parse_rfc3339_secs(&self.request_time)
    }
}
