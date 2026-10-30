//! GoldenRules B5：内置 Provider 与 Assemble 组装（**入口层**）。
//!
//! 边界（`ARCHITECTURE.md` §2.2 / 裁决 A-2）：
//! - 取数、拼装、降级登记都在这一层；
//! - 执行内核（`gr-vm`）永远只看到组装好的 `FactSnapshot`；
//! - 组装器不做决策——它只把「取不到 / 取到但不可信」如实登记，由 B4 的
//!   `onMissing` / `onStale` 策略收口（fail-closed 的唯一出口仍然只有一处）。
//!
//! 三类内置 Provider（`PLAN.md` B5）：
//!
//! | 类型 | 场景 | 关键点 |
//! |---|---|---|
//! | `ConstantProvider` | 配置类事实（限额、开关） | 不把业务参数硬编码进规则包 |
//! | `DerivedProvider` | 派生事实（可用额度、综合信号） | 复用 `gr_vm::compile_expr`，与规则同一白名单 |
//! | `HttpProvider` | 外部服务（名单、评分、聚合指标） | 传输层可注入，超时/5xx 落 missing 不落 500 |
//!
//! 所有 Provider 的失败都表达成 `FactSample::Missing`，而不是 `Err`：
//! 取数失败是**事实状态**，不是请求失败（见 `sample.rs` 模块注释）。

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
