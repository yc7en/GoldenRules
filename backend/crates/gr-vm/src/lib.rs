//! GoldenRules B2: IR->bytecode compilation + VM.
//!
//! Layers: `compile` (structure -> instructions), `vm` (instructions -> values), `eval` (values -> hit set and conclusion).
//! This layer **does not know** snapshots, logs, or error degradation - it only knows `slots: &[Value]`, so it can be exhaustively unit tested.

pub mod compile;
pub mod eval;
pub mod vm;

pub use compile::{
    compile_all, compile_expr, compile_ruleset, BinKind, Chunk, Instr, RuleProgram, RuleSetProgram,
};
pub use eval::{evaluate, run_assigns, EvalOutcome, DEFAULT_REASON_CODE};
pub use vm::{Budget, VmError, DIV_SCALE};
