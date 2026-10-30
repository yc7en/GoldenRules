//! B3 验收：注入快照返回完整 DecisionResult + 命中路径；assign 输出变量正确合并；日志满足 A-4。

use gr_contract::*;
use gr_engine::{engine_from_json, DecisionOutcome, Engine, EngineConfig};
use serde_json::json;

fn fixture(name: &str) -> String {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

fn engine() -> Engine {
    engine_from_json(
        &fixture("fact-catalog.json"),
        &[fixture("rule-package.json")],
        EngineConfig::default(),
    )
    .expect("engine")
}

fn snapshot() -> FactSnapshot {
    serde_json::from_str(&fixture("snapshot.json")).expect("snapshot")
}

fn ctx(scene: &str, ruleset: Option<&str>) -> ExecutionContext {
    let mut c = ExecutionContext::new("pay-cn", scene, "2026-10-30T10:00:00Z");
    c.ruleset_id = ruleset.map(|s| s.to_string());
    c
}

fn decide_basic() -> DecisionOutcome {
    engine()
        .decide(&snapshot(), &ctx("withdraw-risk-check", Some("rs-amount-limit")))
        .expect("decide")
}

#[test]
fn snapshot_mode_returns_full_decision_result() {
    let out = decide_basic();
    let r = &out.result;

    assert_eq!(r.result_version, "v1");
    assert_eq!(r.snapshot_id, "fs-20261030-000123");
    assert_eq!(r.ruleset_id, "rs-amount-limit");
    assert_eq!(r.ruleset_version, "rs-20261030-5");
    assert_eq!(r.decision, Decision::Review);
    assert_eq!(r.reason_code, "AMOUNT_REVIEW");
    assert_eq!(r.evaluated_rules, 3);
    assert_eq!(r.hit_rules.len(), 1, "只有 R2002 命中");
    assert_eq!(r.hit_rules[0].rule_id, "R2002");
    assert_eq!(r.hit_rules[0].priority, 80);
    assert_eq!(r.outputs["riskLevel"], Value::Str("MEDIUM".into()));
    assert!(!r.degraded);
    assert!(r.degraded_reasons.is_empty());
    assert_eq!(r.package_id.as_deref(), Some("pkg-withdraw-risk"));
    assert_eq!(r.package_version.as_deref(), Some("v12"));
    assert_eq!(r.executed_at.as_deref(), Some("2026-10-30T10:00:00Z"));
    assert!(r.execution_time_ms.unwrap() >= 0.0);
    assert_eq!(r.engine_version.as_deref(), Some(ENGINE_VERSION));
}

#[test]
fn assign_outputs_use_exact_decimal() {
    let mut snap = snapshot();
    // amount 200000 > limit 100000 → R2001 命中，excessAmount = 100000
    snap.facts.insert("request.amount".into(), json!("200000.55"));
    let e = engine();
    let mut c = ctx("withdraw-risk-check", Some("rs-amount-limit"));
    let out = e.decide(&snap, &c).expect("decide");
    assert_eq!(out.result.decision, Decision::Reject);
    assert_eq!(out.result.reason_code, "AMOUNT_LIMIT_EXCEEDED");
    assert_eq!(
        out.result.outputs["excessAmount"].to_string(),
        "100000.55",
        "金额必须精确，不能出现浮点尾数"
    );

    // VIP + 6000：R2002 与 R2003 同时命中，同名 riskLevel 冲突取高优先级（R2002 = MEDIUM）
    snap.facts.insert("request.amount".into(), json!("6000"));
    snap.facts.insert("customer.level".into(), json!("VIP"));
    c.request_time = "2026-10-30T10:00:01Z".into();
    let out = e.decide(&snap, &c).expect("decide");
    assert_eq!(out.result.hit_rules.len(), 2);
    assert_eq!(out.result.outputs["riskLevel"], Value::Str("MEDIUM".into()));
    // R2003 独有的变量仍然合并进来
    assert_eq!(out.result.outputs["discountAmount"].to_string(), "600.0");
}

#[test]
fn first_strategy_reports_evaluated_truthfully() {
    let mut snap = snapshot();
    snap.facts.insert("sanctions.hit".into(), json!(true));
    let e = engine();
    let out = e
        .decide(&snap, &ctx("withdraw-risk-check", Some("rs-list-check")))
        .expect("decide");
    assert_eq!(out.result.decision, Decision::Reject);
    assert_eq!(out.result.reason_code, "LIST_SANCTION_HIT");
    assert_eq!(out.result.evaluated_rules, 1, "FIRST 命中即停，不该继续扫");
    assert_eq!(out.result.hit_rules.len(), 1);
}

#[test]
fn default_decision_when_nothing_matches() {
    let mut snap = snapshot();
    snap.facts.insert("sanctions.hit".into(), json!(false));
    let e = engine();
    let out = e
        .decide(&snap, &ctx("withdraw-risk-check", Some("rs-list-check")))
        .expect("decide");
    assert_eq!(out.result.decision, Decision::Approve);
    assert_eq!(out.result.reason_code, "DEFAULT_NO_RULE_MATCHED");
    assert!(out.result.hit_rules.is_empty());
    assert_eq!(out.result.evaluated_rules, 2);
    assert!(out.result.outputs.is_empty());
}

#[test]
fn ambiguous_scene_is_rejected_not_guessed() {
    let e = engine();
    let err = e.decide(&snapshot(), &ctx("withdraw-risk-check", None)).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::VersionSelectionConflict);
    assert_eq!(err.error_code.http_status(), 409);
}

#[test]
fn unknown_scene_is_a_real_error() {
    let e = engine();
    let err = e.decide(&snapshot(), &ctx("nope", None)).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::VersionPackageNotFound);
    assert_eq!(err.error_code.http_status(), 404);
}

#[test]
fn decision_log_satisfies_a4_and_masks_pii() {
    let out = decide_basic();
    let log = &out.log;

    // A-4 必含：快照(脱敏) + 事实目录版本 + 规则包版本 + 命中规则 + 输出 + 降级标记 + 引擎版本 + 耗时
    assert_eq!(log.fact_schema_version, "catalog-v2026.10");
    assert_eq!(log.package_version, "v12");
    assert_eq!(log.ruleset_version, "rs-20261030-5");
    assert_eq!(log.hit_rules.len(), 1);
    assert_eq!(log.outputs["riskLevel"], Value::Str("MEDIUM".into()));
    assert!(!log.degraded);
    assert_eq!(log.engine_version, ENGINE_VERSION);
    assert!(log.execution_time_ms >= 0.0);

    // PII 掩码：request.amount 是 PII → 前两位 + ***
    assert!(log.masked_fields.contains(&"request.amount".to_string()));
    assert_eq!(log.masked_facts["request.amount"], json!("15***"));
    // 非 PII 字段原样保留
    assert_eq!(log.masked_facts["customer.level"], json!("NORMAL"));
    assert_eq!(log.masked_facts["customer.withdrawCount24h"], json!(2));
}

#[test]
fn snapshot_degraded_facts_flow_into_result() {
    let mut snap = snapshot();
    snap.degraded_facts.push(DegradedFact {
        fact: "customer.withdrawCount24h".into(),
        reason: DegradeReason::Stale,
        fallback: Some("DEFAULT".into()),
    });
    let e = engine();
    let out = e
        .decide(&snap, &ctx("withdraw-risk-check", Some("rs-freq")))
        .expect("decide");
    assert!(out.result.degraded);
    assert_eq!(out.result.degraded_reasons.len(), 1);
    assert_eq!(out.result.degraded_reasons[0].fact, "customer.withdrawCount24h");
    assert_eq!(out.result.degraded_reasons[0].reason, "STALE");
    assert!(out.log.degraded);
}

#[test]
fn ruleset_outputs_are_merged_deterministically() {
    // 自造两个规则同时 assign 同一个变量，验证「同名冲突取最高优先级」
    let catalog = json!({
        "catalogVersion": "c",
        "facts": [
            { "fact": "request.amount", "type": "decimal", "required": true, "sensitivity": "INTERNAL" }
        ]
    });
    let pkg = json!({
        "packageId": "pkg-merge", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "outputSchemas": [{
            "outputSchemaId": "merge-out", "outputSchemaVersion": "v1",
            "fields": { "level": { "type": "string" }, "shared": { "type": "integer" } }
        }],
        "rulesets": [{
            "rulesetId": "rs-merge", "version": "v1", "scene": "s",
            "hitStrategy": "ALL", "defaultDecision": "REJECT",
            "outputSchemaId": "merge-out", "outputSchemaVersion": "v1",
            "slotTable": [{ "name": "request.amount", "type": "decimal" }],
            "rules": [
                { "ruleId": "R-high", "priority": 100,
                  "when": { "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] },
                  "then": { "decision": "APPROVE", "reasonCode": "OK",
                            "assign": [
                              { "var": "level", "expr": { "type": "string", "value": "HIGH" } },
                              { "var": "shared", "expr": { "type": "integer", "value": 1 } }
                            ] } },
                { "ruleId": "R-low", "priority": 10,
                  "when": { "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] },
                  "then": { "decision": "REVIEW", "reasonCode": "AMOUNT_REVIEW",
                            "assign": [
                              { "var": "level", "expr": { "type": "string", "value": "LOW" } },
                              { "var": "shared", "expr": { "type": "integer", "value": 1 } }
                            ] } }
            ]
        }]
    });

    let engine = engine_from_json(&catalog.to_string(), &[pkg.to_string()], EngineConfig::default())
        .expect("engine");
    let snap: FactSnapshot = serde_json::from_value(json!({
        "snapshotId": "fs-merge",
        "tenantId": "t",
        "scene": "s",
        "factSchemaVersion": "c",
        "facts": { "request.amount": "10" }
    }))
    .expect("snapshot");

    let out = engine
        .decide(&snap, &ExecutionContext::new("t", "s", "2026-10-30T10:00:00Z"))
        .expect("decide");
    assert_eq!(out.result.hit_rules.len(), 2);
    assert_eq!(out.result.outputs["level"], Value::Str("HIGH".into()), "同名冲突取最高优先级");
    assert_eq!(out.result.outputs["shared"], Value::Int(1), "同名同值幂等");
    assert_eq!(out.result.decided_priority(), 100);
}

trait DecidedPriority {
    fn decided_priority(&self) -> i64;
}

impl DecidedPriority for DecisionResult {
    fn decided_priority(&self) -> i64 {
        self.hit_rules.iter().map(|h| h.priority).max().unwrap_or(0)
    }
}
