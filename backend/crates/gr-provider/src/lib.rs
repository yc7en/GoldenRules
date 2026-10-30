//! GoldenRules B5: built-in Providers and the Assemble assembler (**entry layer**).
//!
//! Boundary (`ARCHITECTURE.md` §2.2 / ruling A-2):
//! - fact fetching, assembling and degradation bookkeeping all live in this layer;
//! - the execution kernel (`gr-vm`) only ever sees an assembled `FactSnapshot`;
//! - the assembler does not make decisions -- it records "could not fetch / fetched but untrustworthy" faithfully, and B4's
//!   `onMissing` / `onStale` strategies close it out (the single exit for fail-closed remains in one place).
//!
//! Three kinds of built-in Provider (`PLAN.md` B5):
//!
//! | Kind | Scene | Key point |
//! |---|---|---|
//! | `ConstantProvider` | configuration facts (limits, switches) | keeps business parameters out of the rule package |
//! | `DerivedProvider` | derived facts (available credit, composite signals) | reuses `gr_vm::compile_expr`, same whitelist as rules |
//! | `HttpProvider` | external services (lists, scores, aggregate metrics) | the transport is injectable; timeouts / 5xx land as missing, not 500 |
//!
//! Every Provider failure is expressed as `FactSample::Missing` rather than `Err`:
//! a fetch failure is a **fact state**, not a request failure (see the `sample.rs` module comment).

pub mod assemble;
pub mod constant;
pub mod derived;
pub mod http;
pub mod sample;
pub mod spec;

pub use assemble::{Assembler, ASSEMBLER_VERSION};
pub use constant::ConstantProvider;
pub use derived::DerivedProvider;
pub use http::{HttpProvider, StdTransport, Transport, TransportError};
pub use sample::{FactMeta, FactSample, JsonMap, Provider, ProviderCtx};
pub use spec::{AssemblerSpec, ProviderSpec, SceneSpec};
