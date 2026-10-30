//! Static constant Provider: configuration-type facts (limits, switches, product parameters).
//!
//! What these facts share is "delivered once, constant for the whole scene", and the worst thing you can do is hard-code them into rules --
//! hard-coding into rules means changing one limit requires republishing the rule package, and rule packages go through approval.

use crate::sample::{FactSample, Provider, ProviderCtx};
use std::collections::BTreeMap;

pub struct ConstantProvider {
    name: String,
    facts: Vec<(String, serde_json::Value)>,
    provides: Vec<String>,
}

impl ConstantProvider {
    pub fn new(
        name: impl Into<String>,
        facts: BTreeMap<String, serde_json::Value>,
    ) -> Self {
        let provides: Vec<String> = facts.keys().cloned().collect();
        let facts: Vec<(String, serde_json::Value)> = facts.into_iter().collect();
        Self { name: name.into(), facts, provides }
    }
}

impl Provider for ConstantProvider {
    fn name(&self) -> &str {
        &self.name
    }

    fn provides(&self) -> &[String] {
        &self.provides
    }

    fn fetch(&self, _ctx: &ProviderCtx<'_>) -> Vec<(String, FactSample)> {
        self.facts
            .iter()
            .map(|(fact, value)| (fact.clone(), FactSample::value(value.clone())))
            .collect()
    }
}
