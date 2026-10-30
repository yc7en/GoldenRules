//! B4 · errors and degradation (fail-closed)
//!
//! Highest-priority boundary (`contracts/05` §1):
//! - **Contract violation** (wrong type, missing required field, snapshot mismatch) -> becomes a **REJECT decision** (`CONTRACT_VIOLATION`),
//!   carrying `degraded=true` and the concrete reason, and **never returns an HTTP 500**;
//! - **Service-level failure** (timeout, overload, internal bug) -> a real error, and the caller fails closed.
//!
//! Validation is staged (`contracts/05` §6.1): the entry validates only **the fields this ruleset consumes** (equivalent to a "pre-generated validator");
//! it does not do a full reflective JSON Schema validation -- at tens of thousands of QPS that is performance suicide.

use gr_contract::{
    Decision, DegradedReason, ErrorCode, ErrorContract, ExecutionContext, FactCatalog, FactSnapshot,
    FactSchema, OnMissing, OutputSchema, Value,
};
use gr_ir::LoadedRuleSet;
use std::collections::{BTreeMap, BTreeSet};

use crate::slots::{self, SlotFill};

/// Product of entry preparation: either evaluation may continue, or it has already been judged as "must become REJECT".
#[derive(Debug)]
pub struct Prepared {
    pub fill: SlotFill,
    /// Contract violations (non-empty -> go straight to a CONTRACT_VIOLATION decision)
    pub violations: Vec<DegradedReason>,
    /// Slots with `onMissing: SKIP`
    pub skip_slots: BTreeSet<u32>,
    /// Scene fallback for `onMissing: FALLBACK_DECISION`
    pub fallback: Option<Decision>,
}

pub const CONTRACT_VIOLATION_REASON: &str = "CONTRACT_VIOLATION";

/// Runtime entry validation + missing-value strategy (`contracts/05` §4.1 / §6.2).
pub fn prepare(
    snapshot: &FactSnapshot,
    ctx: &ExecutionContext,
    rs: &LoadedRuleSet,
    catalog: &FactCatalog,
    config: &crate::EngineConfig,
) -> Prepared {
    let mut violations = Vec::new();
    let mut skip_slots = BTreeSet::new();
    let mut fallback: Option<Decision> = None;

    // (1) Context / snapshot consistency (`contracts/02` §6.1): the EC wins at runtime
    if snapshot.tenant_id != ctx.tenant_id {
        violations.push(violation(
            "tenantId",
            ErrorCode::ContractContextSnapshotMismatch,
            format!("快照 tenantId={} 与上下文 {} 不一致", snapshot.tenant_id, ctx.tenant_id),
        ));
    }
    if snapshot.scene != ctx.scene {
        violations.push(violation(
            "scene",
            ErrorCode::ContractContextSnapshotMismatch,
            format!("快照 scene={} 与上下文 {} 不一致", snapshot.scene, ctx.scene),
        ));
    }

    // (2) Snapshot size (`contracts/02` §3.4)
    let size = serde_json::to_vec(snapshot).map(|v| v.len()).unwrap_or(0);
    if size > config.max_snapshot_bytes {
        violations.push(violation(
            "_snapshot",
            ErrorCode::ContractSnapshotTooLarge,
            format!("快照 {size} 字节，超过上限 {}", config.max_snapshot_bytes),
        ));
    }

    // (3) Unknown fields (strict mode rejects, lenient mode ignores)
    if config.strict_unknown_fields {
        for fact in snapshot.facts.keys() {
            if !rs.slot_index.contains_key(fact) && catalog.get(fact).is_none() {
                violations.push(violation(
                    fact,
                    ErrorCode::ContractUnknownField,
                    "事实目录中不存在该字段（严格模式拒绝）".to_string(),
                ));
            }
        }
    }

    // (4) Type validation + missing-value strategy
    let mut fill = slots::fill(rs, snapshot, catalog);
    for (slot, fact, err) in fill.type_errors.clone() {
        violations.push(violation(
            &fact,
            ErrorCode::ContractTypeError,
            format!("字段类型不符（slot {slot}）：{err}"),
        ));
    }

    let missing: Vec<u32> = fill.missing.iter().copied().collect();
    for slot in missing {
        let fact = rs.slot_table[slot as usize].name.clone();
        let schema = catalog.get(&fact);
        let strategy = schema.map(|s| s.on_missing).unwrap_or(OnMissing::Error);

        match strategy {
            OnMissing::Error => {
                let required = schema.map(|s| s.required).unwrap_or(true);
                violations.push(violation(
                    &fact,
                    ErrorCode::ContractRequiredMissing,
                    format!("必填字段缺失（required={required}）"),
                ));
            }
            OnMissing::Default => match default_for(schema) {
                Some(raw) => {
                    if slots::apply_default(&mut fill, rs, slot, raw) {
                        // The degraded reason is aggregated by the caller
                    } else {
                        violations.push(violation(
                            &fact,
                            ErrorCode::ContractTypeError,
                            "默认值无法按声明类型解析".to_string(),
                        ));
                    }
                }
                None => violations.push(violation(
                    &fact,
                    ErrorCode::ContractRequiredMissing,
                    "onMissing=DEFAULT 但字段未声明 default".to_string(),
                )),
            },
            OnMissing::Skip => {
                skip_slots.insert(slot);
            }
            OnMissing::FallbackDecision => {
                fallback = Some(
                    rs.fallback_decision
                        .clone()
                        .unwrap_or_else(|| rs.default_decision.clone()),
                );
            }
        }
    }

    Prepared { fill, violations, skip_slots, fallback }
}

fn default_for(schema: Option<&FactSchema>) -> Option<&serde_json::Value> {
    let schema = schema?;
    if schema.has_default {
        schema.default.as_ref()
    } else {
        None
    }
}

fn violation(fact: &str, code: ErrorCode, detail: String) -> DegradedReason {
    DegradedReason {
        fact: fact.to_string(),
        reason: "CONTRACT_VIOLATION".to_string(),
        fallback: None,
        code: Some(code.as_str().to_string()),
        detail: Some(detail),
    }
}

/// Facts filled with a default because they were missing / degraded (go into `degradedReasons`, reason=PARTIAL, `contracts/02` §3.2)
pub fn defaulted_reasons(fill: &SlotFill, rs: &LoadedRuleSet) -> Vec<DegradedReason> {
    fill.defaulted
        .keys()
        .map(|slot| DegradedReason {
            fact: rs.slot_table[*slot as usize].name.clone(),
            reason: "PARTIAL".to_string(),
            fallback: Some("DEFAULT".to_string()),
            code: None,
            detail: Some("缺失，按 onMissing=DEFAULT 取默认值".to_string()),
        })
        .collect()
}

/// Exit validation (`contracts/05` §7): cross constraints + outputs against the scene schema.
///
/// A non-empty issue list means the caller swaps the result for a `CONTRACT_VIOLATION` decision, **never returning silently**.
pub fn validate_exit(
    decision: &Decision,
    reason_code: &str,
    hit_rule_ids: &[String],
    evaluated: usize,
    outputs: &BTreeMap<String, Value>,
    degraded: bool,
    degraded_reasons_len: usize,
    program: &gr_vm::RuleSetProgram,
    schema: Option<&OutputSchema>,
) -> Vec<String> {
    let mut issues = Vec::new();

    // Cross constraint: degraded <=> degradedReasons non-empty
    if degraded != (degraded_reasons_len > 0) {
        issues.push(format!(
            "degraded={degraded} 与 degradedReasons 长度 {degraded_reasons_len} 不一致"
        ));
    }
    if reason_code.trim().is_empty() {
        issues.push("reasonCode 为空".to_string());
    }
    if evaluated < hit_rule_ids.len() {
        issues.push(format!(
            "evaluatedRules={evaluated} 小于命中规则数 {}",
            hit_rule_ids.len()
        ));
    }
    // Every hit rule must actually exist
    for id in hit_rule_ids {
        if !program.programs.iter().any(|p| &p.rule_id == id) {
            issues.push(format!("hitRules 中的规则 {id} 不在规则集内"));
        }
    }
    // decision must be inside "built-in enum ∪ decisions declared by this ruleset" (= registered)
    let declared = program
        .programs
        .iter()
        .any(|p| &p.decision == decision);
    if !decision.is_builtin() && !declared {
        issues.push(format!("decision {decision} 未在规则集内声明（未登记）"));
    }

    // Validate outputs against the scene schema (strict: an unregistered variable is a failure)
    match schema {
        Some(schema) => {
            for (var, value) in outputs {
                match schema.fields.get(var) {
                    None => issues.push(format!("输出变量 {var} 未在 {}/{} 中登记",
                        schema.output_schema_id, schema.output_schema_version)),
                    Some(field) => {
                        if let Err(e) = check_output_type(&field.kind, &field.values, field.nullable, value) {
                            issues.push(format!("输出变量 {var} 类型不符：{e}"));
                        }
                    }
                }
            }
            if issues.is_empty() {
                // Everything matches, continue
            }
        }
        None => {
            if !outputs.is_empty() {
                issues.push(format!(
                    "规则集声明了 {} 个输出变量，但没有绑定 outputSchema（无法校验出口）",
                    outputs.len()
                ));
            }
        }
    }

    issues
}

fn check_output_type(
    kind: &gr_contract::FactKind,
    enum_values: &[String],
    nullable: bool,
    value: &Value,
) -> Result<(), String> {
    if value.is_null() {
        return if nullable { Ok(()) } else { Err("字段不可空但输出为 null".to_string()) };
    }
    use gr_contract::FactKind::*;
    match (kind, value) {
        (Integer, Value::Int(_)) => Ok(()),
        (Decimal, Value::Dec(_)) | (Decimal, Value::Int(_)) => Ok(()),
        (String, Value::Str(_)) => Ok(()),
        (Enum, Value::Str(s)) => {
            if enum_values.is_empty() || enum_values.iter().any(|v| v == s) {
                Ok(())
            } else {
                Err(format!("{s} 不在枚举 {enum_values:?} 内"))
            }
        }
        (Boolean, Value::Bool(_)) => Ok(()),
        (Array, Value::Arr(_)) => Ok(()),
        (k, v) => Err(format!("期望 {}，实际 {}", k.as_str(), v.type_name())),
    }
}

/// Turn a contract violation into a decision: `REJECT` + `CONTRACT_VIOLATION` + degraded (`contracts/05` §1).
pub fn violation_decision(rs: &LoadedRuleSet, violations: Vec<DegradedReason>) -> (Decision, String, Vec<DegradedReason>) {
    let _ = rs;
    (Decision::Reject, CONTRACT_VIOLATION_REASON.to_string(), violations)
}

/// Service-level error: contract violations should not take this path; it only catches internal "should never happen" problems.
pub fn internal_error(msg: impl Into<String>) -> ErrorContract {
    ErrorContract::new(ErrorCode::ExecInternal, msg)
}
