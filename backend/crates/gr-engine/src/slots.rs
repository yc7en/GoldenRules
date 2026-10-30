//! slot 装载：FactSnapshot → `Vec<Value>`（B3 入口层）。
//!
//! 只在入口做一次，执行内核拿到的就是下标数组——这正是「字段引用编译期解析为 slot」
//! 的收益：VM 里没有任何字符串查表。

use gr_contract::{FactCatalog, FactKind, FactSnapshot, Value};
use gr_ir::LoadedRuleSet;
use std::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SlotState {
    /// 快照里有值
    Present,
    /// 快照里没有该字段
    Missing,
}

#[derive(Debug)]
pub struct SlotFill {
    pub values: Vec<Value>,
    /// 缺失 slot（用于剪枝与缺失策略）
    pub missing: BTreeSet<u32>,
    /// 类型不符的 slot（`CONTRACT_TYPE_ERROR` 的原料）
    pub type_errors: Vec<(u32, String, String)>,
    /// 被用默认值兜底的 slot（onMissing=DEFAULT 时由 B4 填）
    pub defaulted: BTreeMap<u32, String>,
}

impl SlotFill {
    pub fn is_missing(&self, slot: u32) -> bool {
        self.missing.contains(&slot)
    }
}

/// 按 slot 表把快照值装进栈。
///
/// 类型按 slot 表声明强校验：字符串 `"1500"` → decimal 是**声明类型**要求的转换，
/// 不是隐式跨类型转换（`contracts/01` §3.3 禁止的是「整数和 decimal 混算要人猜」那种）。
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

/// 用默认值填充（`onMissing: DEFAULT`，B4 用）。按 slot 声明类型解析，失败则不动。
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

/// 便捷：某个 slot 的声明类型
pub fn slot_kind(rs: &LoadedRuleSet, slot: u32) -> Option<FactKind> {
    rs.slot_table.get(slot as usize).map(|s| s.kind)
}
