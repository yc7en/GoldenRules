//! B4 acceptance:
//! - A field type error and a missing required field each return the corresponding error code + a REJECT decision (HTTP is still 200);
//! - Timeout / version conflict return service-level errors (HTTP 503 / 409);
//! - The four onMissing states (ERROR / DEFAULT / SKIP / FALLBACK_DECISION) each behave as specified;
//! - Exit validation failures are not silent; they land as CONTRACT_VIOLATION.

use gr_contract::*;
use gr_engine::{engine_from_json, Engine, EngineConfig};
use serde_json::json;

fn fixture(name: &str) -> String {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

fn engine_with(config: EngineConfig) -> Engine {
    engine_from_json(&fixture("fact-catalog.json"), &[fixture("rule-package.json")], config)
        .expect("engine")
}

fn snapshot() -> FactSnapshot {
    serde_json::from_str(&fixture("snapshot.json")).expect("snapshot")
}

fn ctx(ruleset: &str) -> ExecutionContext {
    let mut c = ExecutionContext::new("pay-cn", "withdraw-risk-check", "2026-10-30T10:00:00Z");
    c.ruleset_id = Some(ruleset.to_string());
    c
}

#[test]
fn type_error_becomes_reject_decision_not_http_500() {
    let mut snap = snapshot();
    snap.facts.insert("request.amount".into(), json!("not-a-number"));
    let e = engine_with(EngineConfig::default());
    let out = e.decide(&snap, &ctx("rs-amount-limit")).expect("契约违规不应是 Err");

    assert_eq!(out.result.decision, Decision::Reject);
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out.result.degraded);
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.code.as_deref() == Some("CONTRACT_TYPE_ERROR")));
    assert_eq!(out.result.evaluated_rules, 0, "校验失败不进入求值");
    assert!(out.result.hit_rules.is_empty());
}

#[test]
fn required_missing_becomes_reject_decision() {
    let mut snap = snapshot();
    snap.facts.remove("sanctions.hit");
    let e = engine_with(EngineConfig::default());
    let out = e.decide(&snap, &ctx("rs-list-check")).expect("decision");

    assert_eq!(out.result.decision, Decision::Reject);
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    let codes: Vec<_> = out
        .result
        .degraded_reasons
        .iter()
        .filter_map(|r| r.code.clone())
        .collect();
    assert!(codes.contains(&"CONTRACT_REQUIRED_MISSING".to_string()), "codes={codes:?}");
    assert!(out.result.degraded_reasons.iter().any(|r| r.fact == "sanctions.hit"));
}

#[test]
fn unknown_field_strict_vs_loose() {
    let mut snap = snapshot();
    snap.facts.insert("request.zzz".into(), json!("x"));

    let strict = engine_with(EngineConfig::default());
    let out = strict.decide(&snap, &ctx("rs-amount-limit")).unwrap();
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.code.as_deref() == Some("CONTRACT_UNKNOWN_FIELD")));

    let loose = engine_with(EngineConfig {
        strict_unknown_fields: false,
        ..EngineConfig::default()
    });
    let out = loose.decide(&snap, &ctx("rs-amount-limit")).unwrap();
    assert_eq!(out.result.decision, Decision::Review, "宽松模式忽略未知字段，正常出决策");
}

#[test]
fn context_snapshot_mismatch_is_rejected() {
    let snap = snapshot();
    let e = engine_with(EngineConfig::default());
    let mut c = ctx("rs-amount-limit");
    c.tenant_id = "other-tenant".into();
    let out = e.decide(&snap, &c).unwrap();
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.code.as_deref() == Some("CONTEXT_SNAPSHOT_MISMATCH")));
}

#[test]
fn gray_tag_and_ruleset_version_are_mutually_exclusive() {
    let snap = snapshot();
    let e = engine_with(EngineConfig::default());
    let mut c = ctx("rs-amount-limit");
    c.ruleset_version = Some("rs-20261030-5".into());
    c.gray_tag = Some("gray-10".into());
    let err = e.decide(&snap, &c).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::VersionSelectionConflict);
    assert_eq!(err.error_code.http_status(), 409);
}

#[test]
fn timeout_is_a_real_error() {
    let snap = snapshot();
    let e = engine_with(EngineConfig { deadline_ms: 0, ..EngineConfig::default() });
    let err = e.decide(&snap, &ctx("rs-amount-limit")).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::ExecTimeout);
    assert_eq!(err.error_code.http_status(), 503);
}

#[test]
fn oversized_snapshot_is_rejected_as_decision() {
    let e = engine_with(EngineConfig { max_snapshot_bytes: 64, ..EngineConfig::default() });
    let out = e.decide(&snapshot(), &ctx("rs-amount-limit")).unwrap();
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.code.as_deref() == Some("CONTRACT_SNAPSHOT_TOO_LARGE")));
}

#[test]
fn on_missing_default_uses_default_and_marks_degraded() {
    let mut snap = snapshot();
    snap.facts.remove("customer.riskScore");
    let e = engine_with(EngineConfig::default());
    let out = e.decide(&snap, &ctx("rs-freq")).unwrap();

    // riskScore missing -> use the default 600 (not < 600, and does not trigger isNull) -> no hit -> default APPROVE
    assert_eq!(out.result.decision, Decision::Approve);
    assert_eq!(out.result.reason_code, "DEFAULT_NO_RULE_MATCHED");
    assert!(out.result.degraded, "用了默认值必须标降级");
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.fact == "customer.riskScore" && r.fallback.as_deref() == Some("DEFAULT")));
}

#[test]
fn on_missing_skip_prunes_rules_referencing_the_fact() {
    let catalog = json!({
        "catalogVersion": "c",
        "facts": [
            { "fact": "request.amount", "type": "decimal", "required": true, "onMissing": "ERROR" },
            { "fact": "customer.riskScore", "type": "integer", "onMissing": "SKIP" }
        ]
    });
    let pkg = json!({
        "packageId": "pkg-skip", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "rulesets": [{
            "rulesetId": "rs-skip", "version": "v1", "scene": "s",
            "hitStrategy": "ALL", "defaultDecision": "APPROVE",
            "slotTable": [
                { "name": "request.amount", "type": "decimal" },
                { "name": "customer.riskScore", "type": "integer" }
            ],
            "rules": [
                { "ruleId": "R-amt", "priority": 100,
                  "when": { "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] },
                  "then": { "decision": "REVIEW", "reasonCode": "AMOUNT_REVIEW" } },
                { "ruleId": "R-score", "priority": 90,
                  "when": { "op": "lt", "args": [{ "fact": "customer.riskScore" }, { "type": "integer", "value": "600" }] },
                  "then": { "decision": "REJECT", "reasonCode": "SCORE_LOW" } }
            ]
        }]
    });
    let e = engine_from_json(&catalog.to_string(), &[pkg.to_string()], EngineConfig::default())
        .unwrap();
    let snap: FactSnapshot = serde_json::from_value(json!({
        "snapshotId": "s1", "tenantId": "t", "scene": "s", "factSchemaVersion": "c",
        "facts": { "request.amount": "10" }
    }))
    .unwrap();
    let out = e.decide(&snap, &ExecutionContext::new("t", "s", "2026-10-30T10:00:00Z")).unwrap();

    assert_eq!(out.result.evaluated_rules, 1, "引用 riskScore 的规则被 SKIP");
    assert_eq!(out.result.hit_rules.len(), 1);
    assert_eq!(out.result.hit_rules[0].rule_id, "R-amt");
    assert!(!out.result.degraded, "SKIP 是显式策略，不标降级");
}

#[test]
fn on_missing_fallback_decision_short_circuits() {
    let catalog = json!({
        "catalogVersion": "c",
        "facts": [
            { "fact": "request.amount", "type": "decimal", "required": true, "onMissing": "FALLBACK_DECISION" }
        ]
    });
    let pkg = json!({
        "packageId": "pkg-fb", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "rulesets": [{
            "rulesetId": "rs-fb", "version": "v1", "scene": "s",
            "hitStrategy": "ALL", "defaultDecision": "REJECT", "fallbackDecision": "REVIEW",
            "slotTable": [{ "name": "request.amount", "type": "decimal" }],
            "rules": [
                { "ruleId": "R1", "priority": 10,
                  "when": { "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] },
                  "then": { "decision": "APPROVE", "reasonCode": "OK" } }
            ]
        }]
    });
    let e = engine_from_json(&catalog.to_string(), &[pkg.to_string()], EngineConfig::default())
        .unwrap();
    let snap: FactSnapshot = serde_json::from_value(json!({
        "snapshotId": "s2", "tenantId": "t", "scene": "s", "factSchemaVersion": "c", "facts": {}
    }))
    .unwrap();
    let out = e.decide(&snap, &ExecutionContext::new("t", "s", "2026-10-30T10:00:00Z")).unwrap();

    assert_eq!(out.result.decision, Decision::Review, "走场景 fallbackDecision");
    assert_eq!(out.result.reason_code, "DEGRADE_FALLBACK_DECISION");
    assert!(out.result.degraded);
    assert_eq!(out.result.evaluated_rules, 0);
}

#[test]
fn exit_validation_catches_unregistered_output() {
    let catalog = json!({
        "catalogVersion": "c",
        "facts": [{ "fact": "request.amount", "type": "decimal" }]
    });
    // The rule assigns riskLevel, but the scene output schema does not register it -> exit validation fails
    let pkg = json!({
        "packageId": "pkg-out", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "outputSchemas": [{
            "outputSchemaId": "out-v1", "outputSchemaVersion": "v1",
            "fields": { "level": { "type": "string" } }
        }],
        "rulesets": [{
            "rulesetId": "rs-out", "version": "v1", "scene": "s",
            "hitStrategy": "ALL", "defaultDecision": "REJECT",
            "outputSchemaId": "out-v1", "outputSchemaVersion": "v1",
            "slotTable": [{ "name": "request.amount", "type": "decimal" }],
            "rules": [
                { "ruleId": "R1", "priority": 10,
                  "when": { "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] },
                  "then": { "decision": "APPROVE", "reasonCode": "OK",
                            "assign": [{ "var": "riskLevel", "expr": { "type": "string", "value": "HIGH" } }] } }
            ]
        }]
    });
    let e = engine_from_json(&catalog.to_string(), &[pkg.to_string()], EngineConfig::default())
        .unwrap();
    let snap: FactSnapshot = serde_json::from_value(json!({
        "snapshotId": "s3", "tenantId": "t", "scene": "s", "factSchemaVersion": "c",
        "facts": { "request.amount": "10" }
    }))
    .unwrap();
    let out = e.decide(&snap, &ExecutionContext::new("t", "s", "2026-10-30T10:00:00Z")).unwrap();

    assert_eq!(out.result.decision, Decision::Reject);
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.code.as_deref() == Some("OUTPUT_SCHEMA_VIOLATION")
            && r.detail.as_deref().unwrap_or("").contains("riskLevel")));
    assert!(out.result.outputs.is_empty(), "出口校验失败不返回任何输出变量");
}

#[test]
fn exit_validation_catches_enum_out_of_range() {
    let catalog = json!({
        "catalogVersion": "c",
        "facts": [{ "fact": "request.amount", "type": "decimal" }]
    });
    let pkg = json!({
        "packageId": "pkg-enum", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "outputSchemas": [{
            "outputSchemaId": "out-v1", "outputSchemaVersion": "v1",
            "fields": { "level": { "type": "enum", "values": ["LOW", "HIGH"] } }
        }],
        "rulesets": [{
            "rulesetId": "rs-enum", "version": "v1", "scene": "s",
            "hitStrategy": "ALL", "defaultDecision": "REJECT",
            "outputSchemaId": "out-v1", "outputSchemaVersion": "v1",
            "slotTable": [{ "name": "request.amount", "type": "decimal" }],
            "rules": [
                { "ruleId": "R1", "priority": 10,
                  "when": { "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] },
                  "then": { "decision": "APPROVE", "reasonCode": "OK",
                            "assign": [{ "var": "level", "expr": { "type": "string", "value": "MEDIUM" } }] } }
            ]
        }]
    });
    let e = engine_from_json(&catalog.to_string(), &[pkg.to_string()], EngineConfig::default())
        .unwrap();
    let snap: FactSnapshot = serde_json::from_value(json!({
        "snapshotId": "s4", "tenantId": "t", "scene": "s", "factSchemaVersion": "c",
        "facts": { "request.amount": "10" }
    }))
    .unwrap();
    let out = e.decide(&snap, &ExecutionContext::new("t", "s", "2026-10-30T10:00:00Z")).unwrap();
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|r| r.detail.as_deref().unwrap_or("").contains("MEDIUM")));
}
