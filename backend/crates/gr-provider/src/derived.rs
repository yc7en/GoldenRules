//! 派生 Provider：由其他事实算出新事实（如 `availableLimit = dailyLimit - todayUsed`）。
//!
//! ## 为什么走 VM 而不是在这里写算术
//!
//! 派生逻辑本质是 DSL 表达式，如果这里另写一套求值，就会出现「规则里能算的和派生能算的不一样」
//! ——两套语义迟早漂移。这里把表达式交给 `gr_vm::compile_expr`，与规则**共用同一个编译器与
//! 白名单**：派生事实不可能比规则多出算力（无循环、无副作用、步数受预算限制）。
//!
//! ## 依赖顺序即语义
//!
//! 依赖事实在装配期已经就位（`ProviderCtx.facts`）。因此**Provider 的声明顺序是语义的一部分**，
//! 上游 Provider 必须先于下游派生 Provider（装载期用 `dependsOn` 校验能否满足，不做拓扑排序——
//! 排序会让「为什么这条链是这个顺序」变得不可见）。

use crate::sample::{FactSample, Provider, ProviderCtx};
use gr_contract::{Expr, FactCatalog, FactKind, MissingReason, Value};
use gr_vm::{compile_expr, Chunk};
use std::collections::{BTreeMap, BTreeSet};

pub struct DerivedProvider {
    name: String,
    provides: Vec<String>,
    /// `dependsOn` 的顺序即 slot 下标（编译期定死，运行期不查表）
    deps: Vec<(String, FactKind)>,
    items: Vec<DerivedItem>,
    budget_steps: u64,
}

struct DerivedItem {
    fact: String,
    chunk: Chunk,
    /// 本表达式实际用到哪些依赖 slot（按需判定可算性，不是「缺一个就全废」）
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
    /// `deps` 由调用方按 `dependsOn` 从事实目录解析出类型（装载期做一次）。
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

    /// 声明了但目录里没有的依赖，装载期就报出来（运行期才发现等于每次决策都白跑一遍）。
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
        // 依赖事实按声明类型强校验后再进 slot：与引擎入口 `slots::fill` 同一套规则，
        // 避免「快照里是字符串 "1500"、派生里当成字符串比较」这种双标。
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
                        // 有值但类型不符 → 上游供给错了（PROVIDER_ERROR），不是「没提供」
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
                    // 算不出确定值就不装成 `null`：快照里禁止用 null 表达（`contracts/02` §3.2）
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

/// `Expr::Fact` → `Expr::Slot`（与 `gr-ir::resolve_expr` 同一规则：编译期解析）。
///
/// 派生表达式不接受裸 `slot`：外部配置里出现 slot 下标只能说明「有人在手写编译后 IR」，
/// 那属于越过 DSL 直接构造内部结构，直接拒绝比容忍更安全。
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
