//! B0 验收：`RulePackage` / `FactCatalog` / `FactSnapshot` 的 JSON 往返一致，
//! 且 decimal 不经过二进制浮点（A-5）。
//!
//! 同一份 fixture 也被 Go 侧（`platform/contracts`）读取，用于跨语言 round-trip。

use gr_contract::*;

fn fixture(name: &str) -> serde_json::Value {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    let raw = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"));
    serde_json::from_str(&raw).unwrap_or_else(|e| panic!("parse {path}: {e}"))
}

#[test]
fn rule_package_round_trip() {
    let raw = fixture("rule-package.json");
    let pkg: RulePackage = serde_json::from_value(raw.clone()).expect("deserialize package");
    let back = serde_json::to_value(&pkg).expect("serialize package");
    assert_eq!(raw, back, "RulePackage round-trip 不一致");

    assert_eq!(pkg.package_id, "pkg-withdraw-risk");
    assert_eq!(pkg.rule_count(), 9);
    assert_eq!(pkg.rulesets_of_scene("withdraw-risk-check").count(), 3);
    let rs = pkg.find_ruleset("rs-amount-limit").unwrap();
    assert_eq!(rs.hit_strategy, HitStrategy::All);
    assert_eq!(rs.default_decision, Decision::Reject);
    assert_eq!(rs.slot_table.len(), 4);
}

#[test]
fn fact_catalog_round_trip() {
    let raw = fixture("fact-catalog.json");
    let catalog: FactCatalog = serde_json::from_value(raw.clone()).expect("deserialize catalog");
    let back = serde_json::to_value(&catalog).expect("serialize catalog");
    assert_eq!(raw, back, "FactCatalog round-trip 不一致");
    assert_eq!(catalog.facts.len(), 8);
    let amount = catalog.get("request.amount").unwrap();
    assert_eq!(amount.kind, FactKind::Decimal);
    assert_eq!(amount.sensitivity, Some(Sensitivity::Pii));
    assert_eq!(catalog.get("customer.riskScore").unwrap().on_missing, OnMissing::Default);
}

#[test]
fn snapshot_round_trip() {
    let raw = fixture("snapshot.json");
    let snap: FactSnapshot = serde_json::from_value(raw.clone()).expect("deserialize snapshot");
    let back = serde_json::to_value(&snap).expect("serialize snapshot");
    assert_eq!(raw, back, "FactSnapshot round-trip 不一致");
    assert_eq!(snap.facts["request.amount"], serde_json::json!("1500"));
}

#[test]
fn decimal_is_exact_no_float() {
    // 0.1 + 0.2 必须是精确 0.3，不是 0.30000000000000004
    let a = Literal::dec("0.1");
    let b = Literal::dec("0.2");
    let sum = a.value.as_number().unwrap() + b.value.as_number().unwrap();
    assert_eq!(sum.to_string(), "0.3");

    // 大额金额不丢精度
    let big: Value = serde_json::from_value(serde_json::json!("9999999999999999.99")).unwrap();
    assert_eq!(big.to_string(), "9999999999999999.99");

    // 序列化后仍是字符串
    let json = serde_json::to_value(&big).unwrap();
    assert_eq!(json, serde_json::json!("9999999999999999.99"));
}

#[test]
fn decision_result_shape_matches_contract() {
    let result = DecisionResult {
        result_version: RESULT_VERSION.to_string(),
        snapshot_id: "fs-1".into(),
        trace_id: "trace-1".into(),
        ruleset_id: "rs-1".into(),
        ruleset_version: "rs-v1".into(),
        decision: Decision::Reject,
        reason_code: "AMOUNT_LIMIT_EXCEEDED".into(),
        reason_message: None,
        hit_rules: vec![HitRule {
            rule_id: "R2001".into(),
            rule_version: Some("v4".into()),
            priority: 100,
            reason_code: "AMOUNT_LIMIT_EXCEEDED".into(),
            decision: Some(Decision::Reject),
        }],
        evaluated_rules: 3,
        outputs: [("excessAmount".to_string(), Value::Dec("500".parse().unwrap()))].into(),
        output_schema_id: None,
        output_schema_version: None,
        degraded: false,
        degraded_reasons: vec![],
        execution_time_ms: Some(0.4),
        executed_at: Some("2026-10-30T10:00:00Z".into()),
        engine_version: Some(ENGINE_VERSION.into()),
        package_id: None,
        package_version: None,
    };
    let json = serde_json::to_value(&result).unwrap();
    assert_eq!(json["decision"], serde_json::json!("REJECT"));
    // decimal 出口仍是字符串，消费方（JS/Go）不会拿到 1e-7 之类的浮点
    assert_eq!(json["outputs"]["excessAmount"], serde_json::json!("500"));
    // 契约要求：命中列表里没有冗余 matched 字段
    assert!(json["hitRules"][0].get("matched").is_none());
}

#[test]
fn error_code_mapping() {
    assert_eq!(ErrorCode::ContractTypeError.http_status(), 500);
    assert!(ErrorCode::ContractRequiredMissing.is_contract_violation());
    assert_eq!(ErrorCode::ExecTimeout.http_status(), 503);
    assert!(!ErrorCode::ExecTimeout.is_contract_violation());
}
