//! Slot loading: FactSnapshot -> `Vec<Value>` (B3 entry layer).
//!
//! Done exactly once at the entry, so the execution kernel receives an array of indices -- that is precisely the payoff
//! of "field references compile down to slots": the VM never does a string lookup.

use gr_contract::{FactCatalog, FactKind, FactSnapshot, Value};
use gr_ir::LoadedRuleSet;
use std::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SlotState {
    /// A value is present in the snapshot
    Present,
    /// The snapshot does not contain this field
    Missing,
}

#[derive(Debug)]
pub struct SlotFill {
    pub values: Vec<Value>,
    /// Missing slots (used for pruning and the missing-value strategy)
    pub missing: BTreeSet<u32>,
    /// Slots with a type mismatch (the raw material for `CONTRACT_TYPE_ERROR`)
    pub type_errors: Vec<(u32, String, String)>,
    /// Slots filled with a default (filled by B4 when onMissing=DEFAULT)
    pub defaulted: BTreeMap<u32, String>,
}

impl SlotFill {
    pub fn is_missing(&self, slot: u32) -> bool {
        self.missing.contains(&slot)
    }
}

/// Load the snapshot values into the stack according to the slot table.
///
/// Types are checked strictly against the slot table declaration: converting the string `"1500"` to decimal is a coercion **required by the declared type**,
/// not an implicit cross-type conversion (what `contracts/01` §3.3 forbids is "mixing integers and decimals and making a human guess").
pub fn fill(rs: &LoadedRuleSet, snapshot: &FactSnapshot, _catalog: &FactCatalog) -> SlotFill {
    let mut values = Vec::with_capacity(rs.slot_table.len());
    let mut missing = BTreeSet::new();
    let mut type_errors = Vec::new();

    for (idx, slot) in rs.slot_table.iter().enumerate() {
        let idx = idx as u32;
        match snapshot.facts.get(&slot.name) {
            Some(raw) => match slot.kind.coerce(raw) {
                Ok(v) => values.push(v),
                Err(e) => {
                    type_errors.push((idx, slot.name.clone(), e.to_string()));
                    values.push(Value::Null);
                    missing.insert(idx);
                }
            },
            None => {
                values.push(Value::Null);
                missing.insert(idx);
            }
        }
    }

    SlotFill { values, missing, type_errors, defaulted: BTreeMap::new() }
}

/// Fill with the default value (used by B4 for `onMissing: DEFAULT`). Parsed as the slot's declared type; leaves it untouched on failure.
pub fn apply_default(
    fill: &mut SlotFill,
    rs: &LoadedRuleSet,
    slot: u32,
    raw: &serde_json::Value,
) -> bool {
    let Some(kind) = slot_kind(rs, slot) else { return false };
    match kind.coerce(raw) {
        Ok(v) => {
            fill.values[slot as usize] = v;
            fill.missing.remove(&slot);
            fill.defaulted.insert(slot, "DEFAULT".to_string());
            true
        }
        Err(_) => false,
    }
}

/// Convenience: the declared type of a slot
pub fn slot_kind(rs: &LoadedRuleSet, slot: u32) -> Option<FactKind> {
    rs.slot_table.get(slot as usize).map(|s| s.kind)
}
