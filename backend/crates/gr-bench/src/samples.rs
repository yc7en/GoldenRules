//! B12 样本生成：确定性地产出百条 / 千条规则包与配套快照。
//!
//! 为什么用生成器而不是提交两个大 JSON：
//! - 千条规则包几百 KB，进仓库后每次改一行都要 diff 一个巨型文件；
//! - 生成器把「样本长什么样」写成可读代码，审阅者看 20 行就知道压的是什么；
//! - 种子固定 → 每次生成的包字节一致，基线数字可比。

use serde_json::{json, Value};

/// 压测场景名（与 catalog 里的字段配套）
pub const BENCH_SCENE: &str = "bench";

pub fn ruleset_id(rules: usize) -> String {
    format!("rs-bench-{rules}")
}

/// 造一个含 `rules` 条规则的规则包。
///
/// 每条规则都是「金额阈值 × 次数阈值」的两段 `and`，刻意让一半规则命中：
/// 全不命中只能压到「扫描 + 比较」，全命中会把输出合并也压进去，混合更接近真实流量。
pub fn bench_package(rules: usize) -> Value {
    let mut items = Vec::with_capacity(rules);
    for i in 0..rules {
        let amount = 1000 + i as u64;
        let count = (i % 20) as u64 + 1;
        // 每两条规则里有一条的金额阈值低于快照金额（1500），因此会命中
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

/// 压测用事实目录：只保留样本规则引用到的 4 个事实（其余与压测无关）。
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

/// 压测用快照（金额 1500、24h 次数 2 → 约一半规则命中）。
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
