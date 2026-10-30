//! 执行上下文（`contracts/02` §5 / §6.2）
//!
//! `requestTime` 是唯一时间基准（A-5）：引擎不读系统时钟，回放与测试因此可复现。

use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ExecutionContext {
    pub tenant_id: String,
    pub scene: String,
    /// RFC3339（UTC），唯一时间基准
    pub request_time: String,
    /// 只选规则集版本（不可与 grayTag 同传，`contracts/02` §6.2）
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ruleset_version: Option<String>,
    /// 灰度标签（与 rulesetVersion 互斥）
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub gray_tag: Option<String>,
    /// 指定规则包版本（缺省=当前激活版本）
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub package_version: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub trace_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub idempotency_key: Option<String>,
}

impl ExecutionContext {
    pub fn new(
        tenant_id: impl Into<String>,
        scene: impl Into<String>,
        request_time: impl Into<String>,
    ) -> Self {
        Self {
            tenant_id: tenant_id.into(),
            scene: scene.into(),
            request_time: request_time.into(),
            ruleset_version: None,
            gray_tag: None,
            package_version: None,
            trace_id: None,
            idempotency_key: None,
        }
    }

    pub fn request_time_secs(&self) -> Option<i64> {
        crate::time::parse_rfc3339_secs(&self.request_time)
    }
}
