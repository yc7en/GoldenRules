//! Output variables (assign) and merge rules (`contracts/01` §3.2.3 / `contracts/03` §2.1).
//!
//! The merge is **deterministic**: walk the hit rules in "priority descending -> reasonCode ascending -> ruleId ascending" order,
//! and a variable with the same name **first writer wins** (= the value from the highest-priority hit rule); same name with the same value is idempotent.

use gr_contract::{ErrorContract, Value};
use gr_vm::{run_assigns, Budget, RuleSetProgram};
use std::collections::BTreeMap;

/// Deterministic order of hit rules (used to decide the winner when variables with the same name collide).
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

/// Aggregate the `assign` results of all hit rules.
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
                // Same name, same value: idempotent
                Some(existing) if values_equal(existing, &value) => {}
                // Same name, conflicting value: first writer wins = the highest-priority hit rule
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
