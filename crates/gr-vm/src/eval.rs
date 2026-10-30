//! 命中策略与结论选取（`contracts/01` §3.4.1 / `contracts/05` §8）。

use crate::compile::RuleSetProgram;
use crate::vm::{run, Budget, VmError};
use gr_contract::{Decision, ErrorContract, HitStrategy, Value};

/// 执行结果（不含 `DecisionResult` 的组装与输出变量合并——那是 B3 的活）。
#[derive(Clone, Debug)]
pub struct EvalOutcome {
    /// 命中规则下标，顺序 = 求值顺序（优先级序）
    pub hits: Vec<u32>,
    /// 实际求值条数（剪枝跳过的规则不计入）
    pub evaluated: usize,
    pub decision: Decision,
    pub reason_code: String,
    pub reason_message: Option<String>,
    /// 是否走了 `defaultDecision` 兜底
    pub default_used: bool,
    pub steps_used: u64,
}

pub const DEFAULT_REASON_CODE: &str = "DEFAULT_NO_RULE_MATCHED";

/// 按命中策略求值。
///
/// - `FIRST`：按优先级序，首条命中即停（后续规则不执行，`evaluated` 如实反映）；
/// - `ALL` / `PRIORITY`：全部求值，命中集合相同，差别只在结论选取方式（这里两者等价）。
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

/// 结论选取：
/// - 取**最高优先级**命中规则；
/// - 同优先级按 `reasonCode` 稳定排序取首个（`contracts/01` §3.4.2），再按 ruleId 兜底保证确定性。
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

/// 求一条命中规则的输出变量（B3 负责合并）。
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
