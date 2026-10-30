//! DSL / rule package structures (`contracts/01` §3.2 / §4.2 / §6)
//!
//! One hard rule: **a ruleset is the unit of modularity; expressions/decision tables are merely
//! its notation** (`contracts/01` §3.1, `设计教训.md` anti-pattern 2). The data structures contain no
//! "table module" and no "tree module": decision tables are expanded at compile time into an equivalent rule list (B8 does the expansion, this layer only knows rule lists).

use crate::expr::Expr;
use crate::ty::{FactKind, Literal};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

// ---------------------------------------------------------------------------
// Decision enum (`contracts/03` §3 / `contracts/05` §2)
// ---------------------------------------------------------------------------

/// Business conclusion. Degradation is an orthogonal dimension (`degraded`) and does not belong here (`contracts/05` §2).
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Decision {
    Approve,
    Reject,
    Review,
    Route,
    /// Scene-defined (must be registered in the output schema / registry)
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
// Hit strategy (`contracts/01` §3.4.1)
// ---------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum HitStrategy {
    /// Evaluate everything and collect all hits (default, `contracts/01` §3.4.1)
    All,
    /// In priority order, stop at the very first hit
    First,
    /// Evaluate all, then take the highest-priority hit
    Priority,
}

impl Default for HitStrategy {
    fn default() -> Self {
        HitStrategy::All
    }
}

// ---------------------------------------------------------------------------
// Rules
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
// slot table (`contracts/01` §4.2)
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
// Ruleset
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
// Test cases (`contracts/01` §6.1, a first-class member of the rule package)
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
    /// The ruleset this test case targets (`contracts/01` §6.1).
    ///
    /// Required when a scene carries several rulesets: P0 does not orchestrate multiple
    /// rulesets, so leaving it out means "no idea whom to ask", and the gate rejects it explicitly (instead of picking one at random and producing a seemingly passing result).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ruleset_id: Option<String>,
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
// Scene output schema (`contracts/03` §5.1)
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
// Rule package (`contracts/01` §6)
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
    /// Scene output schema (frozen with the package, used for output validation; moves to the registry at P1)
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub output_schemas: Vec<OutputSchema>,
    /// Package-level slot table (optional). When a ruleset carries its own slot table, the ruleset one wins.
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

/// Convenience: build a literal expression
pub fn lit(l: Literal) -> Expr {
    Expr::Literal(l)
}
