//! DSL / 规则包结构（`contracts/01` §3.2 / §4.2 / §6）
//!
//! 一条硬约定：**规则集是模块单位，表达式/决策表只是它的写法**（`contracts/01` §3.1，
//! `设计教训.md` 反模式 2）。数据结构里不存在「表模块」和「树模块」，决策表在编译期
//! 就展开成等价规则列表（B8 做展开，本层只认规则列表）。

use crate::expr::Expr;
use crate::ty::{FactKind, Literal};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

// ---------------------------------------------------------------------------
// 决策枚举（`contracts/03` §3 / `contracts/05` §2）
// ---------------------------------------------------------------------------

/// 业务结论。降级走正交维度 `degraded`，不进这里（`contracts/05` §2）。
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Decision {
    Approve,
    Reject,
    Review,
    Route,
    /// 场景自定义（须在输出 schema / 注册表登记）
    Custom(String),
}

impl Decision {
    pub fn as_str(&self) -> &str {
        match self {
            Decision::Approve => "APPROVE",
            Decision::Reject => "REJECT",
            Decision::Review => "REVIEW",
            Decision::Route => "ROUTE",
            Decision::Custom(s) => s,
        }
    }

    pub fn is_builtin(&self) -> bool {
        !matches!(self, Decision::Custom(_))
    }
}

impl std::fmt::Display for Decision {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

impl Serialize for Decision {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for Decision {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let s = String::deserialize(d)?;
        Ok(match s.as_str() {
            "APPROVE" => Decision::Approve,
            "REJECT" => Decision::Reject,
            "REVIEW" => Decision::Review,
            "ROUTE" => Decision::Route,
            other => Decision::Custom(other.to_string()),
        })
    }
}

// ---------------------------------------------------------------------------
// 命中策略（`contracts/01` §3.4.1）
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum HitStrategy {
    /// 求值全部，收集全部命中（默认，`contracts/01` §3.4.1）
    All,
    /// 按优先级序，首条命中即停
    First,
    /// 求值后取最高优先级命中
    Priority,
}

impl Default for HitStrategy {
    fn default() -> Self {
        HitStrategy::All
    }
}

// ---------------------------------------------------------------------------
// 规则
// ---------------------------------------------------------------------------

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Assign {
    #[serde(rename = "var")]
    pub var: String,
    pub expr: Expr,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Then {
    pub decision: Decision,
    pub reason_code: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason_message: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub assign: Vec<Assign>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Rule {
    pub rule_id: String,
    #[serde(default)]
    pub version: Option<String>,
    #[serde(default)]
    pub scene: Option<String>,
    #[serde(default)]
    pub priority: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub risk_level: Option<String>,
    pub when: Expr,
    pub then: Then,
}

// ---------------------------------------------------------------------------
// slot 表（`contracts/01` §4.2）
// ---------------------------------------------------------------------------

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SlotDef {
    pub name: String,
    #[serde(rename = "type")]
    pub kind: FactKind,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fact_version: Option<String>,
}

pub type SlotTable = Vec<SlotDef>;

// ---------------------------------------------------------------------------
// 规则集
// ---------------------------------------------------------------------------

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RuleSet {
    pub ruleset_id: String,
    #[serde(default)]
    pub version: Option<String>,
    pub scene: String,
    #[serde(default)]
    pub hit_strategy: HitStrategy,
    pub default_decision: Decision,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fallback_decision: Option<Decision>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_schema_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_schema_version: Option<String>,
    #[serde(default)]
    pub rules: Vec<Rule>,
    #[serde(default)]
    pub slot_table: SlotTable,
}

// ---------------------------------------------------------------------------
// 测试用例（`contracts/01` §6.1，规则包一等成员）
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TestCaseStatus {
    PendingReview,
    Approved,
    Rejected,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TestCase {
    pub test_id: String,
    pub scene: String,
    pub input: BTreeMap<String, serde_json::Value>,
    pub expect: Expectation,
    #[serde(default)]
    pub author: Option<String>,
    #[serde(default)]
    pub status: Option<TestCaseStatus>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Expectation {
    pub decision: Decision,
    pub reason_code: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub outputs: BTreeMap<String, crate::value::Value>,
}

// ---------------------------------------------------------------------------
// 场景输出 schema（`contracts/03` §5.1）
// ---------------------------------------------------------------------------

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OutputField {
    #[serde(rename = "type")]
    pub kind: FactKind,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub values: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub unit: Option<String>,
    #[serde(default, skip_serializing_if = "crate::ty::is_false")]
    pub nullable: bool,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OutputSchema {
    pub output_schema_id: String,
    pub output_schema_version: String,
    pub fields: BTreeMap<String, OutputField>,
}

// ---------------------------------------------------------------------------
// 规则包（`contracts/01` §6）
// ---------------------------------------------------------------------------

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PackageMetadata {
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub created_by: Option<String>,
    #[serde(default)]
    pub risk_level: Option<String>,
    #[serde(default)]
    pub changelog: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RulePackage {
    pub package_id: String,
    pub package_version: String,
    pub fact_schema_version: String,
    pub dsl_version: String,
    pub ir_version: String,
    #[serde(default)]
    pub rulesets: Vec<RuleSet>,
    #[serde(default)]
    pub tests: Vec<TestCase>,
    /// 场景输出 schema（随包冻结，供出口校验；P1 迁注册中心）
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub output_schemas: Vec<OutputSchema>,
    /// 包级 slot 表（可选）。规则集自带 slot 表时以规则集为准。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub slot_table: SlotTable,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub signature: Option<String>,
    #[serde(default)]
    pub metadata: PackageMetadata,
}

impl RulePackage {
    pub fn find_ruleset(&self, ruleset_id: &str) -> Option<&RuleSet> {
        self.rulesets.iter().find(|r| r.ruleset_id == ruleset_id)
    }

    pub fn rulesets_of_scene<'a>(&'a self, scene: &'a str) -> impl Iterator<Item = &'a RuleSet> {
        self.rulesets.iter().filter(move |r| r.scene == scene)
    }

    pub fn rule_count(&self) -> usize {
        self.rulesets.iter().map(|r| r.rules.len()).sum()
    }

    pub fn output_schema(&self, id: &str, version: &str) -> Option<&OutputSchema> {
        self.output_schemas
            .iter()
            .find(|s| s.output_schema_id == id && s.output_schema_version == version)
    }
}

/// 便捷：构造字面量表达式
pub fn lit(l: Literal) -> Expr {
    Expr::Literal(l)
}
