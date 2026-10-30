//! Provider 的声明式配置（`--providers` 指向的 JSON）。
//!
//! 之所以用配置而不是让业务方写 Rust：Provider 组合是**场景级**的，属运营可调的接线，
//! 不是编译期知识。配置在装载期做全量校验（目录/重复/依赖），运行期零反射。

use crate::sample::JsonMap;
use gr_contract::Expr;
use serde::Deserialize;
use std::collections::BTreeMap;

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AssemblerSpec {
    #[serde(default)]
    pub scenes: Vec<SceneSpec>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SceneSpec {
    pub scene: String,
    /// **声明顺序即执行顺序**：派生 Provider 依赖的事实必须由更早的 Provider（或调用方入参）提供。
    #[serde(default)]
    pub providers: Vec<ProviderSpec>,
}

// 注意：枚举上的 `rename_all` 只改**变体名**，变体内部的字段要靠 `rename_all_fields`
// （否则 `dependsOn` / `timeoutMs` 会被静默忽略——这正是本文件第一版踩的坑）。
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase", rename_all_fields = "camelCase", tag = "kind")]
pub enum ProviderSpec {
    /// 静态常量：配置类事实
    Static {
        #[serde(default)]
        name: Option<String>,
        #[serde(default)]
        facts: JsonMap,
    },
    /// 派生表达式：由其他事实算出新事实
    Derived {
        #[serde(default)]
        name: Option<String>,
        #[serde(default)]
        facts: BTreeMap<String, Expr>,
        /// 表达式可引用的事实，顺序即 slot 下标
        #[serde(default)]
        depends_on: Vec<String>,
    },
    /// HTTP 拉取：外部服务供给
    Http {
        #[serde(default)]
        name: Option<String>,
        url: String,
        /// 事实名 → 响应体取值路径（`""`/`"$"` = 响应体本身是事实名→值的对象）
        #[serde(default)]
        facts: BTreeMap<String, String>,
        #[serde(default)]
        timeout_ms: Option<u64>,
    },
}

impl ProviderSpec {
    pub fn kind(&self) -> &'static str {
        match self {
            ProviderSpec::Static { .. } => "static",
            ProviderSpec::Derived { .. } => "derived",
            ProviderSpec::Http { .. } => "http",
        }
    }

    pub fn name(&self) -> Option<&str> {
        match self {
            ProviderSpec::Static { name, .. }
            | ProviderSpec::Derived { name, .. }
            | ProviderSpec::Http { name, .. } => name.as_deref(),
        }
    }

    /// 声明供给的事实名（装载期用于目录校验与重复检测）。
    pub fn declared_facts(&self) -> Vec<String> {
        match self {
            ProviderSpec::Static { facts, .. } => facts.keys().cloned().collect(),
            ProviderSpec::Derived { facts, .. } => facts.keys().cloned().collect(),
            ProviderSpec::Http { facts, .. } => facts.keys().cloned().collect(),
        }
    }
}
