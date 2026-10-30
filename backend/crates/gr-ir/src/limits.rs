//! 加载期资源上限（`contracts/01` §4.5 / `contracts/02` §3.4）
//!
//! B11 会把这些上限做成运行期可配 + 执行隔离；B1 先在**加载期**把明显越界的包挡在门外，
//! 这样 VM 永远不会拿到一个「必然拖垮共享服务」的包。

#[derive(Clone, Debug)]
pub struct LoadLimits {
    pub max_rules_per_package: usize,
    pub max_rules_per_ruleset: usize,
    pub max_expr_depth: usize,
    pub max_expr_nodes: usize,
}

impl Default for LoadLimits {
    fn default() -> Self {
        Self {
            max_rules_per_package: 1000,
            max_rules_per_ruleset: 1000,
            max_expr_depth: 64,
            max_expr_nodes: 1000,
        }
    }
}

impl LoadLimits {
    /// 测试/压测用：放宽规则数上限（仍保留表达式上限）。
    pub fn for_bench() -> Self {
        Self { max_rules_per_package: 100_000, ..Self::default() }
    }
}
