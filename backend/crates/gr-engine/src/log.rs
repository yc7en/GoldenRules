//! 决策日志的脱敏视图（`contracts/03` §6.2，A-4）。
//!
//! 日志只追加、不可篡改；这里产出的是**脱敏后**的快照视图，原始快照另存（P1 加密 + 权限）。

use gr_contract::{FactCatalog, FactSnapshot, Sensitivity};
use std::collections::BTreeMap;

/// PII 掩码：字符串保留前 2 个字符，其余一律 `***`；非字符串整体 `***`。
/// 目的不是"可还原"，而是"能看出有这个字段、值不外泄"。
pub fn mask_value(raw: &serde_json::Value) -> serde_json::Value {
    match raw {
        serde_json::Value::String(s) => {
            let head: String = s.chars().take(2).collect();
            serde_json::json!(format!("{head}***"))
        }
        serde_json::Value::Null => serde_json::Value::Null,
        _ => serde_json::json!("***"),
    }
}

/// 生成脱敏快照 + 被脱敏字段清单。
pub fn mask_snapshot(
    snapshot: &FactSnapshot,
    catalog: &FactCatalog,
) -> (BTreeMap<String, serde_json::Value>, Vec<String>) {
    let index = catalog.index();
    let mut masked = BTreeMap::new();
    let mut masked_fields = Vec::new();

    for (fact, raw) in &snapshot.facts {
        let pii = index
            .get(fact.as_str())
            .map(|f| f.sensitivity == Some(Sensitivity::Pii))
            .unwrap_or(false);
        if pii {
            masked.insert(fact.clone(), mask_value(raw));
            masked_fields.push(fact.clone());
        } else {
            masked.insert(fact.clone(), raw.clone());
        }
    }
    (masked, masked_fields)
}
