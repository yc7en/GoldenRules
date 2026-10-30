//! GoldenRules B2：IR→字节码编译 + VM。
//!
//! 分层：`compile`（结构 → 指令）、`vm`（指令 → 值）、`eval`（值 → 命中集合与结论）。
//! 这一层**不认识**快照、日志、错误降级——它只认 `slots: &[Value]`，因此可被单测穷举。

pub mod compile;
pub mod eval;
pub mod vm;

pub use compile::{
    compile_all, compile_expr, compile_ruleset, BinKind, Chunk, Instr, RuleProgram, RuleSetProgram,
};
pub use eval::{evaluate, run_assigns, EvalOutcome, DEFAULT_REASON_CODE};
pub use vm::{Budget, VmError, DIV_SCALE};
