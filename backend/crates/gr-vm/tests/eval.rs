//! B2 acceptance:
//! - full scan of a thousand rules < 300μs (asserted in release);
//! - decimal arithmetic has no float error;
//! - unit coverage for all three hit strategies, FIRST / ALL / PRIORITY;
//! - whitelist and short-circuit semantics.

use gr_contract::*;
use gr_ir::{load, load_from_json, LoadLimits};
use gr_vm::{compile_ruleset, evaluate, run_assigns, Budget, RuleSetProgram};
use serde_json::json;
use std::time::Instant;

fn fixture(name: &str) -> String {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

fn load_pkg() -> gr_ir::LoadedPackage {
    load_from_json(&fixture("rule-package.json"), &LoadLimits::default()).expect("load")
}

fn slots(values: &[(&str, Value)]) -> Vec<Value> {
    values.iter().map(|(_, v)| v.clone()).collect()
}

// ---------------------------------------------------------------------------
// Real rule packages
// ---------------------------------------------------------------------------

#[test]
fn evaluates_amount_limit_ruleset_with_all_strategy() {
    let pkg = load_pkg();
    let rs = pkg.ruleset("rs-amount-limit").unwrap();
    let program = compile_ruleset(rs).expect("compile");

    // request.amount=1500 / limit=100000 / level=NORMAL / currency=CNY
    let slot_values = slots(&[
        ("request.amount", Value::Dec("1500".parse().unwrap())),
        ("config.withdrawDailyLimit", Value::Dec("100000".parse().unwrap())),
        ("customer.level", Value::Str("NORMAL".into())),
        ("request.currency", Value::Str("CNY".into())),
    ]);
    let mut budget = Budget::new(100_000);
    let outcome =
        evaluate(&program, &slot_values, "2026-10-30T10:00:00Z", &[0, 1, 2], &mut budget)
            .expect("evaluate");

    assert_eq!(outcome.evaluated, 3);
    assert_eq!(outcome.hits, vec![1], "只有 R2002（amount>1000）命中");
    assert_eq!(outcome.decision, Decision::Review);
    assert_eq!(outcome.reason_code, "AMOUNT_REVIEW");

    // assign: riskLevel = "MEDIUM"
    let assigns = run_assigns(&program, 1, &slot_values, "2026-10-30T10:00:00Z", &mut budget)
        .expect("assign");
    assert_eq!(assigns, vec![("riskLevel".to_string(), Value::Str("MEDIUM".into()))]);
}

#[test]
fn first_strategy_stops_at_first_hit() {
    let pkg = load_pkg();
    let rs = pkg.ruleset("rs-list-check").unwrap();
    let program = compile_ruleset(rs).expect("compile");

    // sanctions hit -> only 1 rule is evaluated (R1001 has the highest priority)
    let hit = slots(&[("sanctions.hit", Value::Bool(true)), ("request.currency", Value::Str("CNY".into()))]);
    let mut budget = Budget::new(1000);
    let out = evaluate(&program, &hit, "", &[0, 1], &mut budget).unwrap();
    assert_eq!(out.evaluated, 1);
    assert_eq!(out.reason_code, "LIST_SANCTION_HIT");
    assert_eq!(out.decision, Decision::Reject);

    // no hit -> the defaultDecision fallback (APPROVE) is used, reasonCode is DEFAULT_NO_RULE_MATCHED
    let miss = slots(&[("sanctions.hit", Value::Bool(false)), ("request.currency", Value::Str("CNY".into()))]);
    let mut budget = Budget::new(1000);
    let out = evaluate(&program, &miss, "", &[0, 1], &mut budget).unwrap();
    assert_eq!(out.evaluated, 2);
    assert!(out.default_used);
    assert_eq!(out.decision, Decision::Approve);
    assert_eq!(out.reason_code, "DEFAULT_NO_RULE_MATCHED");
}

#[test]
fn priority_and_all_pick_highest_priority_hit() {
    let pkg = load_pkg();
    let rs = pkg.ruleset("rs-amount-limit").unwrap();
    let program = compile_ruleset(rs).expect("compile");

    // VIP + 6000: R2002(REVIEW,80) and R2003(APPROVE,60) both hit -> take R2002
    let v = slots(&[
        ("request.amount", Value::Dec("6000".parse().unwrap())),
        ("config.withdrawDailyLimit", Value::Dec("100000".parse().unwrap())),
        ("customer.level", Value::Str("VIP".into())),
        ("request.currency", Value::Str("CNY".into())),
    ]);
    let mut budget = Budget::new(100_000);
    let out = evaluate(&program, &v, "", &[0, 1, 2], &mut budget).unwrap();
    assert_eq!(out.hits, vec![1, 2]);
    assert_eq!(out.reason_code, "AMOUNT_REVIEW");

    // ALL and PRIORITY are equivalent in hit set and conclusion (the two spellings in contract §3.4.1)
    let mut pkg2 = load_pkg();
    let rs2 = pkg2.rulesets.get_mut("rs-amount-limit").unwrap();
    std::sync::Arc::get_mut(rs2).unwrap().hit_strategy = HitStrategy::Priority;
    let program2 = compile_ruleset(rs2).expect("compile");
    let mut budget = Budget::new(100_000);
    let out2 = evaluate(&program2, &v, "", &[0, 1, 2], &mut budget).unwrap();
    assert_eq!(out2.hits, out.hits);
    assert_eq!(out2.decision, out.decision);
}

// ---------------------------------------------------------------------------
// Precision and evaluation semantics
// ---------------------------------------------------------------------------

fn program_from(rules: serde_json::Value, strategy: &str, default: &str, slots_def: serde_json::Value) -> RuleSetProgram {
    let pkg = json!({
        "packageId": "pkg-test",
        "packageVersion": "v1",
        "factSchemaVersion": "c1",
        "dslVersion": "1.0",
        "irVersion": "1.0",
        "metadata": {},
        "rulesets": [{
            "rulesetId": "rs-test",
            "version": "rs-v1",
            "scene": "s",
            "hitStrategy": strategy,
            "defaultDecision": default,
            "slotTable": slots_def,
            "rules": rules
        }]
    });
    let loaded = load_from_json(&pkg.to_string(), &LoadLimits::default()).expect("load");
    let rs = loaded.ruleset("rs-test").unwrap();
    compile_ruleset(rs).expect("compile")
}

fn rule(assigns: serde_json::Value, when: serde_json::Value) -> serde_json::Value {
    json!({
        "ruleId": "R1",
        "priority": 100,
        "when": when,
        "then": { "decision": "APPROVE", "reasonCode": "OK", "assign": assigns }
    })
}

#[test]
fn decimal_arithmetic_has_no_float_error() {
    let program = program_from(
        json!([rule(
            json!([{ "var": "sum", "expr": { "op": "add", "args": [
                { "type": "decimal", "value": "0.1" }, { "type": "decimal", "value": "0.2" }
            ]}}]),
            json!({ "type": "boolean", "value": true })
        )]),
        "ALL",
        "REJECT",
        json!([{ "name": "request.amount", "type": "decimal" }]),
    );
    let mut budget = Budget::new(1000);
    let assigns = run_assigns(&program, 0, &[Value::Null], "", &mut budget).unwrap();
    assert_eq!(assigns[0].1.to_string(), "0.3", "0.1+0.2 必须精确为 0.3");
}

#[test]
fn division_rounds_to_8_places() {
    let program = program_from(
        json!([rule(
            json!([{ "var": "v", "expr": { "op": "div", "args": [
                { "type": "decimal", "value": "1" }, { "type": "decimal", "value": "3" }
            ]}}]),
            json!({ "type": "boolean", "value": true })
        )]),
        "ALL",
        "REJECT",
        json!([{ "name": "request.amount", "type": "decimal" }]),
    );
    let mut budget = Budget::new(1000);
    let assigns = run_assigns(&program, 0, &[Value::Null], "", &mut budget).unwrap();
    assert_eq!(assigns[0].1.to_string(), "0.33333333");
}

#[test]
fn numeric_string_literal_sugar_works_in_arithmetic() {
    // the contracts/01 §3.2.3 example multiplies by a string literal such as "0.1"
    let program = program_from(
        json!([rule(
            json!([{ "var": "cut", "expr": { "op": "mul", "args": [
                { "fact": "request.amount" }, { "type": "string", "value": "0.1" }
            ]}}]),
            json!({ "op": "gt", "args": [{ "fact": "request.amount" }, { "type": "decimal", "value": "0" }] })
        )]),
        "ALL",
        "REJECT",
        json!([{ "name": "request.amount", "type": "decimal" }]),
    );
    let mut budget = Budget::new(1000);
    let assigns = run_assigns(&program, 0, &[Value::Dec("1500".parse().unwrap())], "", &mut budget)
        .unwrap();
    assert_eq!(assigns[0].1.to_string(), "150.0");
}

#[test]
fn logical_operators_short_circuit() {
    // the right branch is a type error (comparing a string with a number), but the left branch is false -> the right branch must not be evaluated
    let program = program_from(
        json!([{
            "ruleId": "R1",
            "priority": 100,
            "when": { "op": "and", "args": [
                { "op": "eq", "args": [{ "fact": "request.currency" }, { "type": "string", "value": "USD" }] },
                { "op": "gt", "args": [{ "fact": "request.currency" }, { "type": "integer", "value": 1 }] }
            ]},
            "then": { "decision": "REJECT", "reasonCode": "X" }
        }]),
        "ALL",
        "APPROVE",
        json!([
            { "name": "request.currency", "type": "enum" },
            { "name": "request.amount", "type": "decimal" }
        ]),
    );
    let mut budget = Budget::new(1000);
    let out = evaluate(&program, &[Value::Str("CNY".into()), Value::Null], "", &[0], &mut budget)
        .expect("短路后不应报错");
    assert!(out.hits.is_empty());
    assert_eq!(out.decision, Decision::Approve);
}

#[test]
fn whitelist_is_enforced_at_compile_time() {
    let pkg = json!({
        "packageId": "p", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "rulesets": [{
            "rulesetId": "rs", "version": "v1", "scene": "s", "hitStrategy": "ALL",
            "defaultDecision": "REJECT",
            "slotTable": [{ "name": "request.amount", "type": "decimal" }],
            "rules": [{
                "ruleId": "R1", "priority": 1, "when": { "type": "boolean", "value": true },
                "then": { "decision": "APPROVE", "reasonCode": "OK" }
            }]
        }]
    });
    // unknown op
    let mut bad = pkg.clone();
    bad["rulesets"][0]["rules"][0]["when"] =
        json!({ "op": "evalJs", "args": [{ "fact": "request.amount" }] });
    let loaded = load_from_json(&bad.to_string(), &LoadLimits::default()).unwrap();
    let err = compile_ruleset(loaded.ruleset("rs").unwrap()).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::DslOpNotAllowed);

    // unknown function
    let mut bad = pkg.clone();
    bad["rulesets"][0]["rules"][0]["when"] = json!({ "op": "call", "func": "httpGet", "args": [] });
    let loaded = load_from_json(&bad.to_string(), &LoadLimits::default()).unwrap();
    let err = compile_ruleset(loaded.ruleset("rs").unwrap()).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::DslFnNotFound);

    // wrong argument count
    let mut bad = pkg;
    bad["rulesets"][0]["rules"][0]["when"] = json!({ "op": "call", "func": "abs", "args": [] });
    let loaded = load_from_json(&bad.to_string(), &LoadLimits::default()).unwrap();
    let err = compile_ruleset(loaded.ruleset("rs").unwrap()).unwrap_err();
    assert_eq!(err.error_code, ErrorCode::DslArityMismatch);
}

#[test]
fn whitelist_functions_behave() {
    let program = program_from(
        json!([rule(
            json!([
                { "var": "isHit", "expr": { "op": "call", "func": "inSet", "args": [
                    { "fact": "request.currency" },
                    { "type": "array", "value": ["CNY", "USD"] }
                ]}},
                { "var": "rounded", "expr": { "op": "call", "func": "round", "args": [
                    { "type": "decimal", "value": "0.125" }, { "type": "integer", "value": 2 }
                ]}},
                { "var": "fallback", "expr": { "op": "call", "func": "coalesce", "args": [
                    { "fact": "customer.riskScore" }, { "type": "integer", "value": 600 }
                ]}},
                { "var": "prefix", "expr": { "op": "call", "func": "substr", "args": [
                    { "type": "string", "value": "ABCDE" }, { "type": "integer", "value": 1 }, { "type": "integer", "value": 3 }
                ]}},
                { "var": "nowIso", "expr": { "op": "call", "func": "now", "args": [] } }
            ]),
            json!({ "type": "boolean", "value": true })
        )]),
        "ALL",
        "REJECT",
        json!([
            { "name": "request.currency", "type": "enum" },
            { "name": "customer.riskScore", "type": "integer" }
        ]),
    );
    let mut budget = Budget::new(10_000);
    let assigns = run_assigns(
        &program,
        0,
        &[Value::Str("CNY".into()), Value::Null],
        "2026-10-30T10:00:00Z",
        &mut budget,
    )
    .unwrap();
    let map: std::collections::BTreeMap<_, _> = assigns.into_iter().collect();
    assert_eq!(map["isHit"], Value::Bool(true));
    assert_eq!(map["rounded"].to_string(), "0.13");
    assert_eq!(map["fallback"], Value::Int(600));
    assert_eq!(map["prefix"], Value::Str("BCD".into()));
    assert_eq!(map["nowIso"], Value::Str("2026-10-30T10:00:00Z".into()));
}

// ---------------------------------------------------------------------------
// Performance: full scan of a thousand rules < 300μs (release)
// ---------------------------------------------------------------------------

fn bench_program(n: usize) -> RuleSetProgram {
    let rules: Vec<serde_json::Value> = (0..n)
        .map(|i| {
            json!({
                "ruleId": format!("B{i:04}"),
                "priority": (n - i) as i64,
                "when": { "op": "gt", "args": [
                    { "type": "decimal", "value": format!("{}.50", i) },
                    { "fact": "request.amount" }
                ]},
                "then": { "decision": "REVIEW", "reasonCode": format!("BENCH_{i}") }
            })
        })
        .collect();
    let pkg = json!({
        "packageId": "pkg-bench", "packageVersion": "v1", "factSchemaVersion": "c",
        "dslVersion": "1.0", "irVersion": "1.0", "metadata": {},
        "rulesets": [{
            "rulesetId": "rs-bench", "version": "v1", "scene": "bench",
            "hitStrategy": "ALL", "defaultDecision": "REJECT",
            "slotTable": [{ "name": "request.amount", "type": "decimal" }],
            "rules": rules
        }]
    });
    let loaded = load_from_json(&pkg.to_string(), &LoadLimits::for_bench()).expect("load bench");
    compile_ruleset(loaded.ruleset("rs-bench").unwrap()).expect("compile bench")
}

#[test]
fn thousand_rules_scan_under_300_micros() {
    let program = bench_program(1000);
    let plan: Vec<u32> = (0..1000).collect();
    let slot_values = [Value::Dec("99999.99".parse().unwrap())];
    let mut budget = Budget::new(10_000_000);
    // warm-up
    for _ in 0..3 {
        let _ = evaluate(&program, &slot_values, "", &plan, &mut budget).unwrap();
        budget = Budget::new(10_000_000);
    }

    let iterations = 200;
    let started = Instant::now();
    for _ in 0..iterations {
        let mut b = Budget::new(10_000_000);
        let out = evaluate(&program, &slot_values, "", &plan, &mut b).unwrap();
        assert_eq!(out.evaluated, 1000);
    }
    let per = started.elapsed() / iterations as u32;
    println!("千条规则全扫：{per:?}/次（{} 条指令）", program.total_instrs);

    if !cfg!(debug_assertions) {
        assert!(
            per.as_micros() < 300,
            "千条规则全扫耗时 {per:?}，超过 300μs 预算"
        );
    }
}

#[test]
fn pruned_plan_is_cheaper_than_full_scan() {
    let pkg = load_pkg();
    let rs = pkg.ruleset("rs-amount-limit").unwrap();
    let program = compile_ruleset(rs).unwrap();
    // amount is missing (slot 0 missing) -> every rule gated on amount is skipped
    let mut missing = std::collections::BTreeSet::new();
    missing.insert(0u32);
    missing.insert(1u32);
    missing.insert(2u32);
    missing.insert(3u32);
    let plan = rs.plan(&missing);
    assert!(plan.is_empty(), "全部前置事实缺失时不应求值任何规则");
    let mut budget = Budget::new(1000);
    let slot_values = vec![Value::Null; 4];
    let out = evaluate(&program, &slot_values, "", &plan, &mut budget).unwrap();
    assert_eq!(out.evaluated, 0);
    assert!(out.default_used);
    assert_eq!(out.decision, Decision::Reject, "兜底决策遵循规则集 defaultDecision");
}

#[test]
fn loads_1000_rules_via_ir_then_vm() {
    // B1 + B2 joint acceptance: a thousand rules can be loaded and compiled
    let program = bench_program(1000);
    assert_eq!(program.programs.len(), 1000);
    assert!(program.total_instrs > 0);
    let _ = load(&{
        let mut p: RulePackage =
            serde_json::from_str(&fixture("rule-package.json")).expect("parse");
        p.package_version = "v13".into();
        p
    }, &LoadLimits::default())
    .expect("load fixture again");
}
