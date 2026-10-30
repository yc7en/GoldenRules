//! Masked view of the decision log (`contracts/03` §6.2, A-4).
//!
//! The log is append-only and tamper-evident; what we emit here is the **masked** snapshot view, while the raw snapshot is stored separately (P1: encryption + access control).

use gr_contract::{FactCatalog, FactSnapshot, Sensitivity};
use std::collections::BTreeMap;

/// PII mask: keep the first 2 characters of a string and replace the rest with `***`; any non-string becomes `***` wholesale.
/// The goal is not "reversible" but "you can tell the field exists while the value never leaks".
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

/// Produce the masked snapshot + the list of masked fields.
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
