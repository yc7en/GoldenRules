//! GoldenRules 共享契约（B0）
//!
//! 与 `contracts/01/02/03/05` 对齐的**数据形状**，是 Rust 决策服务与 Go 管理平台之间
//! 唯一的共享面。共享以 **JSON 序列化格式**为准（`PLAN.md` §2 B0）：两语言各持绑定，
//! 不共享运行时、不共享进程。
//!
//! 本 crate 只定义结构，不含任何求值/加载逻辑——那是 B1（gr-ir）与 B2（gr-vm）的事。

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

/// DSL / IR 版本（`contracts/01` §3.1 / §4.1）
pub const DSL_VERSION: &str = "1.0";
pub const IR_VERSION: &str = "1.0";

/// 调用方可以按 `resultVersion` 解析（`contracts/03` §9.1）
pub fn supported_result_version(v: &str) -> bool {
    v == RESULT_VERSION
}
