//! 输出变量（assign）与合并规则（`contracts/01` §3.2.3 / `contracts/03` §2.1）。
//!
//! 合并是**确定性**的：按「优先级降序 → reasonCode 升序 → ruleId 升序」遍历命中规则，
//! 同名变量**先写先赢**（= 最高优先级命中规则的值），同名同值幂等。

use gr_contract::{ErrorContract, Value};
use gr_vm::{run_assigns, Budget, RuleSetProgram};
use std::collections::BTreeMap;

/// 命中规则的确定性顺序（同名变量冲突时用它决定谁赢）。
pub fn hit_order(program: &RuleSetProgram, hits: &[u32]) -> Vec<u32> {
    let mut ordered = hits.to_vec();
    ordered.sort_by(|&a, &b| {
        let ra = program.program(a);
        let rb = program.program(b);
        rb.priority
            .cmp(&ra.priority)
            .then_with(|| ra.reason_code.cmp(&rb.reason_code))
            .then_with(|| ra.rule_id.cmp(&rb.rule_id))
    });
    ordered
}

/// 汇总所有命中规则的 `assign`。
pub fn compute_outputs(
    program: &RuleSetProgram,
    hits: &[u32],
    slots: &[Value],
    request_time: &str,
    budget: &mut Budget,
) -> Result<BTreeMap<String, Value>, ErrorContract> {
    let mut outputs: BTreeMap<String, Value> = BTreeMap::new();

    for idx in hit_order(program, hits) {
        let assigns = run_assigns(program, idx, slots, request_time, budget)?;
        for (var, value) in assigns {
            match outputs.get(&var) {
                None => {
                    outputs.insert(var, value);
                }
                // 同名同值：幂等
                Some(existing) if values_equal(existing, &value) => {}
                // 同名冲突：先写先赢 = 最高优先级命中规则
                Some(_) => {}
            }
        }
    }
    Ok(outputs)
}

fn values_equal(a: &Value, b: &Value) -> bool {
    match (a.as_number(), b.as_number()) {
        (Some(x), Some(y)) => x == y,
        _ => a == b,
    }
}
