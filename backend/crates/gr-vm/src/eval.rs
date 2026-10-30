//! Hit strategy and conclusion selection (`contracts/01` §3.4.1 / `contracts/05` §8).

use crate::compile::RuleSetProgram;
use crate::vm::{run, Budget, VmError};
use gr_contract::{Decision, ErrorContract, HitStrategy, Value};

/// Execution result (excluding `DecisionResult` assembly and output variable merging - that is the job of B3).
#[derive(Clone, Debug)]
pub struct EvalOutcome {
    /// Hit rule indices, order = evaluation order (priority order)
    pub hits: Vec<u32>,
    /// Number of rules actually evaluated (rules skipped by pruning are not counted)
    pub evaluated: usize,
    pub decision: Decision,
    pub reason_code: String,
    pub reason_message: Option<String>,
    /// Whether the `defaultDecision` fallback was used
    pub default_used: bool,
    pub steps_used: u64,
}

pub const DEFAULT_REASON_CODE: &str = "DEFAULT_NO_RULE_MATCHED";

/// Evaluate according to the hit strategy.
///
/// - `FIRST`: in priority order, stop at the first hit (later rules are not executed, `evaluated` reflects that honestly);
/// - `ALL` / `PRIORITY`: evaluate everything, the hit set is identical, they differ only in how the conclusion is selected (equivalent here).
pub fn evaluate(
    program: &RuleSetProgram,
    slots: &[Value],
    request_time: &str,
    plan: &[u32],
    budget: &mut Budget,
) -> Result<EvalOutcome, ErrorContract> {
    let mut hits: Vec<u32> = Vec::new();
    let mut evaluated = 0usize;

    for &idx in plan {
        let rule = program.program(idx);
        let value = run(&rule.when, slots, request_time, budget).map_err(|e| {
            annotate(e, &program.ruleset_id, &rule.rule_id, "when")
        })?;
        evaluated += 1;
        if value.is_truthy() {
            hits.push(idx);
            if program.hit_strategy == HitStrategy::First {
                break;
            }
        }
    }

    let steps_used = budget.steps_used;

    if hits.is_empty() {
        return Ok(EvalOutcome {
            hits,
            evaluated,
            decision: program.default_decision.clone(),
            reason_code: DEFAULT_REASON_CODE.to_string(),
            reason_message: None,
            default_used: true,
            steps_used,
        });
    }

    let winner = select_winner(program, &hits);
    let rule = program.program(winner);
    Ok(EvalOutcome {
        hits,
        evaluated,
        decision: rule.decision.clone(),
        reason_code: rule.reason_code.clone(),
        reason_message: rule.reason_message.clone(),
        default_used: false,
        steps_used,
    })
}

/// Conclusion selection:
/// - take the hit rule with the **highest priority**;
/// - for equal priority sort stably by `reasonCode` and take the first (`contracts/01` §3.4.2), then fall back to ruleId for determinism.
fn select_winner(program: &RuleSetProgram, hits: &[u32]) -> u32 {
    let mut best = hits[0];
    for &idx in &hits[1..] {
        let a = program.program(best);
        let b = program.program(idx);
        let better = b.priority > a.priority
            || (b.priority == a.priority
                && (b.reason_code.as_str(), b.rule_id.as_str())
                    < (a.reason_code.as_str(), a.rule_id.as_str()));
        if better {
            best = idx;
        }
    }
    best
}

/// Compute the output variables of one hit rule (B3 takes care of merging).
pub fn run_assigns(
    program: &RuleSetProgram,
    rule_index: u32,
    slots: &[Value],
    request_time: &str,
    budget: &mut Budget,
) -> Result<Vec<(String, Value)>, ErrorContract> {
    let rule = program.program(rule_index);
    let mut out = Vec::with_capacity(rule.assigns.len());
    for (var, chunk) in &rule.assigns {
        let v = run(chunk, slots, request_time, budget)
            .map_err(|e| annotate(e, &program.ruleset_id, &rule.rule_id, "assign"))?;
        out.push((var.clone(), v));
    }
    Ok(out)
}

fn annotate(e: VmError, ruleset_id: &str, rule_id: &str, phase: &str) -> ErrorContract {
    let mut c: ErrorContract = e.into();
    c.details.insert("rulesetId".into(), serde_json::json!(ruleset_id));
    c.details.insert("ruleId".into(), serde_json::json!(rule_id));
    c.details.insert("phase".into(), serde_json::json!(phase));
    c
}
