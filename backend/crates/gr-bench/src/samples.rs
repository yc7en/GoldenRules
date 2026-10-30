//! B12 sample generation: deterministically produce the 100-rule / 1000-rule rule package plus its snapshot.
//!
//! Why a generator instead of committing two big JSON files:
//! - A 1000-rule package is several hundred KB; once in the repo, every one-line change dirties a giant file in the diff;
//! - The generator writes "what the sample looks like" as readable code, so a reviewer sees 20 lines and knows what is being exercised;
//! - Fixed seed -> byte-identical package on every run, so baseline numbers stay comparable.

use serde_json::{json, Value};

/// Load-test scenario name (pairs with the field in the fact catalog)
pub const BENCH_SCENE: &str = "bench";

pub fn ruleset_id(rules: usize) -> String {
    format!("rs-bench-{rules}")
}

/// Build a rule package containing `rules` rules.
///
/// Each rule is an `and` of two segments, "amount threshold x count threshold", deliberately tuned so half of them hit:
/// hitting none only stresses "scan + compare", hitting all also brings output merging in; a mix is closer to real traffic.
pub fn bench_package(rules: usize) -> Value {
    let mut items = Vec::with_capacity(rules);
    for i in 0..rules {
        let amount = 1000 + i as u64;
        let count = (i % 20) as u64 + 1;
        // One of every two rules has an amount threshold below the snapshot amount (1500), so it hits
        let decision = if i % 2 == 0 { "REVIEW" } else { "APPROVE" };
        items.push(json!({
            "ruleId": format!("B{i:04}"),
            "version": "v1",
            "priority": 1000 - (i as i64 % 1000),
            "when": {
                "op": "and",
                "args": [
                    { "op": "gt", "args": [
                        { "fact": "request.amount" },
                        { "type": "decimal", "value": amount.to_string() }
                    ]},
                    { "op": "lt", "args": [
                        { "fact": "customer.withdrawCount24h" },
                        { "type": "integer", "value": count }
                    ]}
                ]
            },
            "then": {
                "decision": decision,
                "reasonCode": format!("BENCH_{i:04}"),
                "reasonMessage": "压测样本规则"
            }
        }));
    }

    json!({
        "packageId": format!("pkg-bench-{rules}"),
        "packageVersion": "v1",
        "factSchemaVersion": "catalog-bench-v1",
        "dslVersion": gr_contract::DSL_VERSION,
        "irVersion": gr_contract::IR_VERSION,
        "rulesets": [{
            "rulesetId": ruleset_id(rules),
            "version": "v1",
            "scene": BENCH_SCENE,
            "hitStrategy": "ALL",
            "defaultDecision": "APPROVE",
            "slotTable": [
                { "name": "request.amount", "type": "decimal", "factVersion": "v1" },
                { "name": "customer.withdrawCount24h", "type": "integer", "factVersion": "v1" }
            ],
            "rules": items
        }]
    })
}

/// Fact catalog for the load test: keep only the 4 facts the sample rules reference (the rest are irrelevant to the bench).
pub fn bench_catalog() -> Value {
    json!({
        "catalogVersion": "catalog-bench-v1",
        "facts": [
            { "fact": "request.amount", "type": "decimal", "unit": "CNY", "required": true, "onMissing": "ERROR" },
            { "fact": "customer.withdrawCount24h", "type": "integer", "unit": "count", "required": true, "onMissing": "ERROR" },
            { "fact": "customer.level", "type": "enum", "enum": ["NORMAL", "VIP"], "onMissing": "ERROR" },
            { "fact": "sanctions.hit", "type": "boolean", "onMissing": "ERROR" }
        ]
    })
}

/// Snapshot for the load test (amount 1500, 24h count 2 -> roughly half of the rules hit).
pub fn bench_snapshot() -> Value {
    json!({
        "snapshotId": "fs-bench-000001",
        "snapshotVersion": "v1",
        "tenantId": "bench",
        "scene": BENCH_SCENE,
        "factSchemaVersion": "catalog-bench-v1",
        "facts": {
            "request.amount": "1500",
            "customer.withdrawCount24h": 2,
            "customer.level": "NORMAL",
            "sanctions.hit": false
        }
    })
}
