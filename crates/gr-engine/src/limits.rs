//! B11 · 执行资源上限与隔离（A-9）
//!
//! 这是**多团队共用的安全阀**，不是性能优化：一个团队写出笛卡尔积式的烂规则，
//! 不能让其他团队一起付代价。
//!
//! ## 三道闸门，三种失败方式
//!
//! | 闸门 | 时机 | 失败形态 |
//! |---|---|---|
//! | 装载期（规则数 / `dependsOn` 深度 / 指令总量 / 内存估算） | 装载 | 真错误，包进不来（`EXEC_RULE_LIMIT_EXCEEDED`） |
//! | 请求期（快照大小 / 指令预算 / 场景超时） | 每次决策 | 契约问题 → REJECT 决策；**超时/超预算 → 真错误** |
//! | 准入（在途请求数） | 进 `decide` 前 | 真错误 `EXEC_OVERLOAD`（503），**不排队** |
//!
//! ## 为什么过载要「快速失败」而不是排队
//!
//! 排队会把「过载」变形成「无界延迟」：调用方以为请求还在处理，超时后重试，
//! 负载进一步升高——雪崩的标准路径。快速失败把选择权交回调用方（fail-closed，D5），
//! 并且**过载是真错误而非 REJECT 决策**：REJECT 是业务结论，过载与业务无关，
//! 混在一起会让调用方把「系统忙」记成「这笔提款被拒绝」。
//!
//! ## 「隔离」落到代码上是两件事
//!
//! 1. **预算不共享**：每次决策一份 `Budget`，一个请求跑满预算不会吞掉别人的额度；
//! 2. **并发不互相蚕食**：全局在途上限之外，每个场景还有自己的在途上限——
//!    否则一个团队的慢场景（比如 20ms 的重规则集）就能吃满全局并发，
//!    把核心场景（5ms）的容量挤掉。

use serde::Serialize;
use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

/// 单场景的限额覆盖（未设置则用全局默认）。
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SceneLimit {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub step_budget: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub deadline_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_inflight: Option<usize>,
}

impl SceneLimit {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn with_deadline_ms(mut self, ms: u64) -> Self {
        self.deadline_ms = Some(ms);
        self
    }

    pub fn with_step_budget(mut self, steps: u64) -> Self {
        self.step_budget = Some(steps);
        self
    }

    pub fn with_max_inflight(mut self, n: usize) -> Self {
        self.max_inflight = Some(n);
        self
    }
}

/// 生效中的限额（算上场景覆盖后的结果），用于自述与排障。
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EffectiveLimits {
    pub scene: String,
    pub step_budget: u64,
    pub deadline_ms: u64,
    pub max_inflight: usize,
}

struct SceneSlot {
    used: AtomicUsize,
    max: usize,
}

impl SceneSlot {
    fn new(max: usize) -> Self {
        Self { used: AtomicUsize::new(0), max }
    }
}

/// 准入闸门：全局在途 + 每场景在途。
pub struct Gate {
    global: AtomicUsize,
    max_global: usize,
    scenes: BTreeMap<String, SceneSlot>,
    fallback: SceneSlot,
}

impl Gate {
    /// `scenes` 传入「已知场景 → 单场景上限」；`max_global` 是所有场景之和的硬顶。
    pub fn new(max_global: usize, scenes: Vec<(String, usize)>) -> Self {
        let scenes = scenes
            .into_iter()
            .map(|(name, max)| (name, SceneSlot::new(max)))
            .collect();
        Self {
            global: AtomicUsize::new(0),
            max_global,
            scenes,
            // 未在配置中声明的场景（例如新增场景还没配限额）走兜底，同样受限
            fallback: SceneSlot::new(usize::MAX),
        }
    }

    fn slot(&self, scene: &str) -> &SceneSlot {
        self.scenes.get(scene).unwrap_or(&self.fallback)
    }

    /// 尝试占一个名额；拿不到就返回 `None`（调用方立刻降级为 503，不等待）。
    pub fn admit(&self, scene: &str) -> Option<Admission<'_>> {
        let slot = self.slot(scene);

        // 先占全局：全局是最外层的硬顶，拿到再试场景，避免场景满了还占着全局名额
        let previous = self.global.fetch_add(1, Ordering::AcqRel);
        if previous >= self.max_global {
            self.global.fetch_sub(1, Ordering::AcqRel);
            return None;
        }

        let scene_prev = slot.used.fetch_add(1, Ordering::AcqRel);
        if scene_prev >= slot.max {
            slot.used.fetch_sub(1, Ordering::AcqRel);
            self.global.fetch_sub(1, Ordering::AcqRel);
            return None;
        }

        Some(Admission { global: &self.global, scene: &slot.used })
    }

    pub fn inflight(&self) -> usize {
        self.global.load(Ordering::Acquire)
    }

    pub fn scene_inflight(&self, scene: &str) -> usize {
        self.slot(scene).used.load(Ordering::Acquire)
    }
}

/// RAII 名额：`decide` 返回时自动归还，包括 panic 展开路径。
pub struct Admission<'a> {
    global: &'a AtomicUsize,
    scene: &'a AtomicUsize,
}

impl Drop for Admission<'_> {
    fn drop(&mut self) {
        self.scene.fetch_sub(1, Ordering::AcqRel);
        self.global.fetch_sub(1, Ordering::AcqRel);
    }
}

/// 一份规则包的资源画像（装载期算一次，运行期不算账）。
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PackageStats {
    pub package_id: String,
    pub package_version: String,
    pub rulesets: usize,
    pub rules: usize,
    pub instrs: usize,
    /// 内存估算（指令 + 常量 + 规则元数据），用于装载期上限判定，不是精确 RSS
    pub est_bytes: usize,
}

/// 指令结构体大小（估算用；不同平台一致，因为字段是定长枚举与 u32）
pub const INSTR_BYTES: usize = 12;
/// 每条规则的元数据估算（rule_id / reason_code / 版本字符串等）
pub const RULE_META_BYTES: usize = 128;

/// 常量值的字节估算（decimal/字符串按内容长度，数组递归）。
pub fn value_bytes(v: &gr_contract::Value) -> usize {
    use gr_contract::Value;
    match v {
        Value::Null => 0,
        Value::Bool(_) => 1,
        Value::Int(_) => 8,
        Value::Dec(d) => 16 + d.to_string().len(),
        Value::Str(s) => 24 + s.len(),
        Value::Arr(items) => 24 + items.iter().map(value_bytes).sum::<usize>(),
    }
}

/// 一个 chunk 的字节估算。
pub fn chunk_bytes(chunk: &gr_vm::Chunk) -> usize {
    chunk.code.len() * INSTR_BYTES + chunk.consts.iter().map(value_bytes).sum::<usize>()
}

/// 组装「已用 / 上限」视图，供健康检查与自述使用。
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LimitsReport {
    pub max_snapshot_bytes: usize,
    pub max_rules_per_package: usize,
    pub max_instrs_per_package: usize,
    pub max_package_bytes: usize,
    pub max_inflight: usize,
    pub inflight: usize,
    pub step_budget: u64,
    pub deadline_ms: u64,
    pub scenes: Vec<EffectiveLimits>,
}

/// 便于 `Engine::new` 里把配置摊平成 Gate 的入参。
pub fn scene_caps(
    default_cap: usize,
    overrides: &BTreeMap<String, SceneLimit>,
) -> Vec<(String, usize)> {
    overrides
        .iter()
        .map(|(scene, limit)| (scene.clone(), limit.max_inflight.unwrap_or(default_cap)))
        .collect()
}

/// 共享只读视图（`Engine` 内部持有）。
pub type SharedGate = Arc<Gate>;
