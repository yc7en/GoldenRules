//! Load-time resource limits (`contracts/01` §4.5 / `contracts/02` §3.4)
//!
//! B11 will make these limits runtime-configurable + execution-isolated; B1 first keeps obviously out-of-bounds packages outside the door at **load time**,
//! so the VM never receives a package that would "inevitably drag down the shared service".

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
    /// For tests/benchmarks: loosen the rule count limit (expression limits still apply).
    pub fn for_bench() -> Self {
        Self { max_rules_per_package: 100_000, ..Self::default() }
    }
}
