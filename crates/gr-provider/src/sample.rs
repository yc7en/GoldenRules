//! Provider 契约：取数的输入视图 + 单事实结果 + `Provider` trait。
//!
//! ## 为什么 `fetch` 不返回 `Result`
//!
//! 取数失败在契约里**不是一个错误，是一个事实状态**（`contracts/02` §3.2）：
//! 超时要落 `missingFacts` + `PROVIDER_TIMEOUT`，由 `onMissing` 策略决定后果
//! （`ERROR` → REJECT / `DEFAULT` → 兜底 / `SKIP` → 跳过规则 / `FALLBACK_DECISION`）。
//! 如果 `fetch` 能返回 `Err`，调用方就会想「整个请求失败」，那恰好绕过了
//! fail-closed 的分级降级——所以失败被强制表达成 `FactSample::Missing`。
//!
//! 只有**装配器自身**的配置错误（场景不存在、Provider 声称供一个目录里没有的事实）
//! 才是 `ErrorContract`，因为那是装载期 bug，不是运行期事实状态。

use gr_contract::{DegradeReason, MissingReason};
use std::collections::BTreeMap;

pub type JsonMap = BTreeMap<String, serde_json::Value>;
pub type FactMeta = BTreeMap<String, serde_json::Value>;

/// 一次取数能看到的只读视图。
#[derive(Clone, Copy, Debug)]
pub struct ProviderCtx<'a> {
    pub tenant_id: &'a str,
    pub scene: &'a str,
    /// 唯一时间基准，取自 `ExecutionContext.request_time`（引擎不读系统时钟，A-5）。
    /// Provider 要取「近 24 小时」这类窗口，也只能按这个时间算。
    pub request_time: &'a str,
    /// 调用方业务入参（Assemble 模式的原始输入）。
    pub inputs: &'a JsonMap,
    /// **此前已装配**的事实。派生 Provider 依赖它，因此 Provider 顺序是语义的一部分。
    pub facts: &'a JsonMap,
}

/// 单个事实的供给结果（三态，与 `contracts/02` §3.2 一一对应）。
#[derive(Clone, Debug, PartialEq)]
pub enum FactSample {
    /// 正常供给。`meta` 用于携带 `eventTime` / `sourceTimestamp` 等（时效计算原料）。
    Value { value: serde_json::Value, meta: Option<FactMeta> },
    /// 没取到。落 `missingFacts`。
    Missing(MissingReason),
    /// 取到了但不可信（超期、上游部分降级）。值仍进 `facts`，同时落 `degradedFacts`。
    Degraded {
        value: serde_json::Value,
        reason: DegradeReason,
        fallback: Option<String>,
        meta: Option<FactMeta>,
    },
}

impl FactSample {
    pub fn value(value: serde_json::Value) -> Self {
        FactSample::Value { value, meta: None }
    }

    /// 附带元数据（如 `eventTime`）。未声明的 meta 键告警不拒绝（`contracts/02` §4）。
    pub fn with_meta(mut self, key: impl Into<String>, value: serde_json::Value) -> Self {
        let key = key.into();
        match &mut self {
            FactSample::Value { meta, .. } | FactSample::Degraded { meta, .. } => {
                meta.get_or_insert_with(BTreeMap::new).insert(key, value);
            }
            FactSample::Missing(_) => {}
        }
        self
    }

    pub fn missing(reason: MissingReason) -> Self {
        FactSample::Missing(reason)
    }

    pub fn degraded(value: serde_json::Value, reason: DegradeReason) -> Self {
        FactSample::Degraded { value, reason, fallback: None, meta: None }
    }
}

/// 取数单元。实现者只做「我负责的事实现在是什么值」，不做决策、不改快照。
pub trait Provider: Send + Sync {
    fn name(&self) -> &str;

    /// 本 Provider 负责的事实名。装配器在装载期用它做目录校验与重复供给检测。
    fn provides(&self) -> &[String];

    /// 取数。返回顺序即装配顺序（同事实先到先得）。
    fn fetch(&self, ctx: &ProviderCtx<'_>) -> Vec<(String, FactSample)>;
}
