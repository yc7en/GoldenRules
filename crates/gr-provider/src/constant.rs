//! 静态常量 Provider：配置类事实（限额、开关、产品参数）。
//!
//! 这类事实的共同点是「一次下发、整场不变」，最忌讳的是硬编码进规则里——
//! 硬编码进规则意味着改一个限额就要重发规则包，而规则包是走审批的。

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
