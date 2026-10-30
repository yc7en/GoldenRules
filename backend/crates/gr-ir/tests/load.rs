//! B1 验收：
//! - 千条规则规则包能加载，slot 解析正确；
//! - 按 scene 选择后只扫相关规则集，前置事实缺失时跳过对应规则。

use gr_contract::*;
use gr_ir::{load, load_from_json, LoadLimits};
use std::time::Instant;

fn fixture(name: &str) -> String {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

fn load_fixture() -> gr_ir::LoadedPackage {
    load_from_json(&fixture("rule-package.json"), &LoadLimits::default()).expect("load package")
}

#[test]
fn loads_package_and_resolves_slots() {
    let pkg = load_fixture();
    assert_eq!(pkg.package_id, "pkg-withdraw-risk");
    assert_eq!(pkg.package_version, "v12");
    assert_eq!(pkg.rule_count, 9);
    assert_eq!(pkg.scene_index.get("withdraw-risk-check").map(Vec::len), Some(3));
    assert_eq!(pkg.scene_index.get("withdraw-rate").map(Vec::len), Some(1));

    let rs = pkg.ruleset("rs-amount-limit").unwrap();
    assert_eq!(rs.rule_count, 3);
    // 字段引用已解析成 slot 下标：request.amount 是 slot 0
    let r2001 = rs.rules.iter().find(|r| r.rule.rule_id == "R2001").unwrap();
    match &r2001.rule.when {
        Expr::Op(op) => {
            assert_eq!(op.op, "gt");
            assert_eq!(op.args[0], Expr::Slot(SlotRef { slot: 0 }));
            assert_eq!(op.args[1], Expr::Slot(SlotRef { slot: 1 }));
        }
        other => panic!("unexpected when: {other:?}"),
    }
    // assign 里的字段引用也被解析
    let assign = &r2001.rule.then.assign[0];
    assert_eq!(first_leaf_slot(&assign.expr), Some(0));
    assert_eq!(r2001.gating_slot, Some(0));
    assert!(r2001.has_assign);

    // decimal 字面量保持字符串精度
    let r2002 = rs.rules.iter().find(|r| r.rule.rule_id == "R2002").unwrap();
    if let Expr::Op(op) = &r2002.rule.when {
        assert_eq!(op.args[1], Expr::Literal(Literal::dec("1000")));
    }
}

#[test]
fn rules_are_sorted_by_priority_then_id() {
    let pkg = load_fixture();
    for rs in pkg.rulesets.values() {
        let mut sorted = rs.rules.clone();
        sorted.sort_by(|a, b| {
            b.rule.priority.cmp(&a.rule.priority).then_with(|| a.rule.rule_id.cmp(&b.rule.rule_id))
        });
        let got: Vec<_> = rs.rules.iter().map(|r| r.rule.rule_id.clone()).collect();
        let want: Vec<_> = sorted.iter().map(|r| r.rule.rule_id.clone()).collect();
        assert_eq!(got, want, "规则集 {} 排序不稳定", rs.ruleset_id);
        for (i, r) in rs.rules.iter().enumerate() {
            assert_eq!(r.index, i as u32);
        }
    }
}

#[test]
fn scene_selection_limits_scan_scope() {
    let pkg = load_fixture();
    // 单场景决策只选一个规则集：withdraw-rate 只有 2 条规则，不会碰 withdraw-risk-check 的 7 条
    let rs = pkg.select_ruleset("withdraw-rate", None, None).expect("select");
    assert_eq!(rs.ruleset_id, "rs-withdraw-rate");
    assert_eq!(rs.rule_count, 2);

    // 一个场景挂多个规则集时，必须显式指定，避免「顺手全跑」
    let err = pkg.select_ruleset("withdraw-risk-check", None, None).unwrap_err();
    assert_eq!(err.issues[0].code, ErrorCode::VersionSelectionConflict);

    let rs = pkg
        .select_ruleset("withdraw-risk-check", Some("rs-freq"), None)
        .expect("select by id");
    assert_eq!(rs.rule_count, 2);
    assert_eq!(rs.version, "rs-20261030-2");
}

#[test]
fn pruning_skips_rules_whose_gate_fact_is_missing() {
    let pkg = load_fixture();
    let rs = pkg.ruleset("rs-freq").unwrap();
    assert_eq!(rs.plan(&Default::default()).len(), 2);

    // riskScore 是 slot 1，缺失 → R3002 被跳过
    let mut missing = std::collections::BTreeSet::new();
    missing.insert(1u32);
    let plan = rs.plan(&missing);
    assert_eq!(plan.len(), 1);
    assert_eq!(rs.rule(plan[0]).rule.rule_id, "R3001");
}

#[test]
fn thousand_rule_package_loads() {
    let mut pkg = RulePackage {
        package_id: "pkg-bench".into(),
        package_version: "v1".into(),
        fact_schema_version: "catalog-v2026.10".into(),
        dsl_version: "1.0".into(),
        ir_version: "1.0".into(),
        rulesets: vec![],
        tests: vec![],
        output_schemas: vec![],
        slot_table: vec![],
        signature: None,
        metadata: PackageMetadata::default(),
    };

    let rules: Vec<Rule> = (0..1000)
        .map(|i| Rule {
            rule_id: format!("B{i:04}"),
            version: Some("v1".into()),
            scene: Some("bench".into()),
            priority: (1000 - i) as i64,
            risk_level: None,
            when: Expr::op(
                "gt",
                vec![
                    Expr::fact("request.amount"),
                    Expr::Literal(Literal::dec(&format!("{}.50", i))),
                ],
            ),
            then: Then {
                decision: Decision::Review,
                reason_code: format!("BENCH_{i}"),
                reason_message: None,
                assign: vec![],
            },
        })
        .collect();

    pkg.rulesets.push(RuleSet {
        ruleset_id: "rs-bench".into(),
        version: Some("rs-v1".into()),
        scene: "bench".into(),
        hit_strategy: HitStrategy::All,
        default_decision: Decision::Reject,
        fallback_decision: None,
        output_schema_id: None,
        output_schema_version: None,
        rules,
        slot_table: vec![SlotDef {
            name: "request.amount".into(),
            kind: FactKind::Decimal,
            fact_version: Some("v2".into()),
        }],
    });

    let started = Instant::now();
    let loaded = load(&pkg, &LoadLimits::for_bench()).expect("load 1000 rules");
    let elapsed = started.elapsed();
    assert_eq!(loaded.rule_count, 1000);
    let rs = loaded.ruleset("rs-bench").unwrap();
    assert_eq!(rs.plan(&Default::default()).len(), 1000);
    // 加载 1000 条（含 slot 解析 + 排序）应在毫秒级
    assert!(elapsed.as_millis() < 200, "load took {elapsed:?}");
}

#[test]
fn rejects_rule_over_package_limit() {
    let pkg = RulePackage {
        package_id: "pkg-big".into(),
        package_version: "v1".into(),
        fact_schema_version: "c".into(),
        dsl_version: "1.0".into(),
        ir_version: "1.0".into(),
        rulesets: vec![RuleSet {
            ruleset_id: "rs".into(),
            version: None,
            scene: "s".into(),
            hit_strategy: HitStrategy::All,
            default_decision: Decision::Reject,
            fallback_decision: None,
            output_schema_id: None,
            output_schema_version: None,
            rules: (0..1001)
                .map(|i| Rule {
                    rule_id: format!("R{i}"),
                    version: None,
                    scene: None,
                    priority: 0,
                    risk_level: None,
                    when: Expr::Literal(Literal::boolean(true)),
                    then: Then {
                        decision: Decision::Approve,
                        reason_code: "OK".into(),
                        reason_message: None,
                        assign: vec![],
                    },
                })
                .collect(),
            slot_table: vec![],
        }],
        tests: vec![],
        output_schemas: vec![],
        slot_table: vec![],
        signature: None,
        metadata: PackageMetadata::default(),
    };
    let err = load(&pkg, &LoadLimits::default()).unwrap_err();
    assert_eq!(err.issues[0].code, ErrorCode::ExecRuleLimitExceeded);
}

#[test]
fn rejects_unknown_fact_reference() {
    let mut pkg: RulePackage =
        serde_json::from_str(&fixture("rule-package.json")).expect("parse fixture");
    // 只把规则里的字段引用写错（slot 表保持正确），模拟「引用了目录里不存在的字段」
    pkg.rulesets[1].rules[0].when = Expr::op(
        "gt",
        vec![Expr::fact("request.amout"), Expr::fact("config.withdrawDailyLimit")],
    );
    let err = load(&pkg, &LoadLimits::default()).unwrap_err();
    assert!(
        err.issues.iter().any(|i| i.code == ErrorCode::IrUnknownFact),
        "expected IR_UNKNOWN_FACT, got {:?}",
        err.issues
    );
}

#[test]
fn rejects_duplicate_slot_and_bad_ir_version() {
    let mut pkg: RulePackage =
        serde_json::from_str(&fixture("rule-package.json")).expect("parse fixture");
    pkg.ir_version = "2.0".into();
    let err = load(&pkg, &LoadLimits::default()).unwrap_err();
    assert_eq!(err.issues[0].code, ErrorCode::IrVersionUnsupported);

    pkg.ir_version = "1.0".into();
    pkg.rulesets[0].slot_table.push(SlotDef {
        name: "sanctions.hit".into(),
        kind: FactKind::Boolean,
        fact_version: None,
    });
    let err = load(&pkg, &LoadLimits::default()).unwrap_err();
    assert_eq!(err.issues[0].code, ErrorCode::IrDuplicateSlot);
}

fn first_leaf_slot(expr: &Expr) -> Option<u32> {
    match expr {
        Expr::Slot(s) => Some(s.slot),
        Expr::Op(o) => o.args.iter().find_map(first_leaf_slot),
        _ => None,
    }
}
