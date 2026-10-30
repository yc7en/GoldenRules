//! Derived Provider: compute new facts from other facts (e.g. `availableLimit = dailyLimit - todayUsed`).
//!
//! ## Why go through the VM instead of writing the arithmetic here
//!
//! Derived logic is essentially a DSL expression; writing a second evaluator here would yield "what a rule can compute differs from what a derivation can compute"
//! -- two sets of semantics drift sooner or later. Here the expression is handed to `gr_vm::compile_expr`, **sharing the same compiler and
//! whitelist** as rules: a derived fact can never gain more computing power than a rule (no loops, no side effects, step count bounded by the budget).
//!
//! ## Dependency order is semantics
//!
//! Dependency facts are already in place at assembly time (`ProviderCtx.facts`). Therefore **the declaration order of Providers is part of the semantics**:
//! an upstream Provider must precede a downstream derived Provider (load time validates satisfiability via `dependsOn` and does no topological sort --
//! sorting would make "why this chain is in this order" invisible).

use crate::sample::{FactSample, Provider, ProviderCtx};
use gr_contract::{Expr, FactCatalog, FactKind, MissingReason, Value};
use gr_vm::{compile_expr, Chunk};
use std::collections::{BTreeMap, BTreeSet};

pub struct DerivedProvider {
    name: String,
    provides: Vec<String>,
    /// The order of `dependsOn` is the slot index (fixed at compile time, no lookup at runtime)
    deps: Vec<(String, FactKind)>,
    items: Vec<DerivedItem>,
    budget_steps: u64,
}

struct DerivedItem {
    fact: String,
    chunk: Chunk,
    /// Which dependency slots this expression actually uses (feasibility is decided on demand, not "one missing kills everything")
    used: BTreeSet<u32>,
}

impl std::fmt::Debug for DerivedProvider {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DerivedProvider")
            .field("name", &self.name)
            .field("provides", &self.provides)
            .field("deps", &self.deps)
            .finish()
    }
}

impl DerivedProvider {
    /// `deps` is resolved by the caller from the fact catalog according to `dependsOn` (done once at load time).
    pub fn new(
        name: impl Into<String>,
        deps: Vec<(String, FactKind)>,
        facts: BTreeMap<String, Expr>,
    ) -> Result<Self, gr_contract::ErrorContract> {
        let provides: Vec<String> = facts.keys().cloned().collect();
        let mut items = Vec::with_capacity(facts.len());
        for (fact, expr) in facts {
            let resolved = resolve(&expr, &deps)
                .map_err(|e| e.with_detail("derivedFact", serde_json::json!(fact)))?;
            let mut used = Vec::new();
            resolved.collect_slots(&mut used);
            let chunk = compile_expr(&resolved)
                .map_err(|e| e.with_detail("derivedFact", serde_json::json!(fact)))?;
            items.push(DerivedItem { fact, chunk, used: used.into_iter().collect() });
        }
        Ok(Self { name: name.into(), provides, deps, items, budget_steps: 10_000 })
    }

    /// Dependencies declared but missing from the catalog are reported at load time (discovering it at runtime means every decision does the work twice for nothing).
    pub fn missing_catalog_facts(&self, catalog: &FactCatalog) -> Vec<String> {
        self.deps
            .iter()
            .filter(|(fact, _)| catalog.get(fact).is_none())
            .map(|(fact, _)| fact.clone())
            .collect()
    }
}

impl Provider for DerivedProvider {
    fn name(&self) -> &str {
        &self.name
    }

    fn provides(&self) -> &[String] {
        &self.provides
    }

    fn fetch(&self, ctx: &ProviderCtx<'_>) -> Vec<(String, FactSample)> {
        // Dependency facts are type-checked against their declared types before entering slots: the same rules as the engine entry `slots::fill`,
        // so we avoid double standards such as "the snapshot has the string "1500" but derivation compares it as a string".
        let mut slots: Vec<Value> = Vec::with_capacity(self.deps.len());
        let mut absent: BTreeSet<u32> = BTreeSet::new();
        let mut broken: BTreeSet<u32> = BTreeSet::new();

        for (idx, (fact, kind)) in self.deps.iter().enumerate() {
            match ctx.facts.get(fact) {
                None => {
                    slots.push(Value::Null);
                    absent.insert(idx as u32);
                }
                Some(raw) => match kind.coerce(raw) {
                    Ok(v) => slots.push(v),
                    Err(_) => {
                        // Has a value but the type does not match -> upstream supplied it wrongly (PROVIDER_ERROR), not "not provided"
                        slots.push(Value::Null);
                        broken.insert(idx as u32);
                    }
                },
            }
        }

        let mut out = Vec::with_capacity(self.items.len());
        for item in &self.items {
            if item.used.iter().any(|s| absent.contains(s)) {
                out.push((item.fact.clone(), FactSample::missing(MissingReason::NotProvided)));
                continue;
            }
            if item.used.iter().any(|s| broken.contains(s)) {
                out.push((item.fact.clone(), FactSample::missing(MissingReason::ProviderError)));
                continue;
            }

            let mut budget = gr_vm::Budget::new(self.budget_steps);
            match gr_vm::vm::run(&item.chunk, &slots, ctx.request_time, &mut budget) {
                Ok(Value::Null) => {
                    // If no definite value can be computed, do not store `null`: expressing values as null is forbidden in snapshots (`contracts/02` §3.2)
                    out.push((item.fact.clone(), FactSample::missing(MissingReason::NotProvided)));
                }
                Ok(v) => match serde_json::to_value(&v) {
                    Ok(json) => out.push((item.fact.clone(), FactSample::value(json))),
                    Err(_) => out.push((
                        item.fact.clone(),
                        FactSample::missing(MissingReason::ProviderError),
                    )),
                },
                Err(_) => out.push((
                    item.fact.clone(),
                    FactSample::missing(MissingReason::ProviderError),
                )),
            }
        }
        out
    }
}

/// `Expr::Fact` -> `Expr::Slot` (same rule as `gr-ir::resolve_expr`: resolved at compile time).
///
/// A derived expression does not accept a bare `slot`: a slot index showing up in external config only means "someone is hand-writing compiled IR";
/// that bypasses the DSL and builds internal structures directly, and rejecting it outright is safer than tolerating it.
fn resolve(
    expr: &Expr,
    deps: &[(String, FactKind)],
) -> Result<Expr, gr_contract::ErrorContract> {
    use gr_contract::ErrorCode;
    match expr {
        Expr::Literal(l) => Ok(Expr::Literal(l.clone())),
        Expr::Slot(s) => Err(gr_contract::ErrorContract::new(
            ErrorCode::IrInvalid,
            format!("派生表达式不接受裸 slot 引用（slot={}），请用 fact 名", s.slot),
        )),
        Expr::Fact(f) => match deps.iter().position(|(name, _)| name == &f.fact) {
            Some(idx) => Ok(Expr::slot(idx as u32)),
            None => Err(gr_contract::ErrorContract::new(
                ErrorCode::IrUnknownFact,
                format!("派生表达式引用了未在 dependsOn 中声明的事实 {}", f.fact),
            )),
        },
        Expr::Op(op) => {
            let mut args = Vec::with_capacity(op.args.len());
            for a in &op.args {
                args.push(resolve(a, deps)?);
            }
            Ok(Expr::Op(gr_contract::OpExpr {
                op: op.op.clone(),
                args,
                negated: op.negated,
                func: op.func.clone(),
            }))
        }
    }
}
