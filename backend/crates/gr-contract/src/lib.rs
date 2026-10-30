//! GoldenRules shared contracts (B0)
//!
//! The **data shapes** aligned with `contracts/01/02/03/05`, the only shared surface between the Rust decision
//! service and the Go management platform. Sharing is defined by the **JSON serialization format**
//! (`PLAN.md` §2 B0): each language keeps its own bindings, no shared runtime, no shared process.
//!
//! This crate defines structures only and holds no evaluation/loading logic - that is the job of B1 (gr-ir) and B2 (gr-vm).

pub mod context;
pub mod decision;
pub mod dsl;
pub mod error;
pub mod expr;
pub mod fact;
pub mod time;
pub mod ty;
pub mod value;

pub use context::ExecutionContext;
pub use decision::{
    DecisionLogEntry, DecisionResult, DegradedReason, HitRule, ENGINE_VERSION, RESULT_VERSION,
};
pub use dsl::{
    Assign, Decision, Expectation, HitStrategy, OutputField, OutputSchema, PackageMetadata, Rule,
    RulePackage, RuleSet, SlotDef, SlotTable, TestCase, TestCaseStatus, Then,
};
pub use error::{ContractIssue, EngineResult, ErrorCode, ErrorContract};
pub use expr::{Expr, FactRef, Func, Op, OpExpr, SlotRef};
pub use fact::{
    Consistency, DegradeReason, DegradedFact, FactCatalog, FactSchema, FactSnapshot, Freshness,
    MissingFact, MissingReason, OnMissing, OnStale, Sensitivity,
};
pub use ty::{CoerceError, FactKind, Literal};
pub use value::Value;

/// DSL / IR versions (`contracts/01` §3.1 / §4.1)
pub const DSL_VERSION: &str = "1.0";
pub const IR_VERSION: &str = "1.0";

/// Callers can parse by `resultVersion` (`contracts/03` §9.1)
pub fn supported_result_version(v: &str) -> bool {
    v == RESULT_VERSION
}
