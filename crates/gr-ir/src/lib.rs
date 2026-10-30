//! B1 · IR 加载与 slot 表
//!
//! 职责只有三件（`PLAN.md` B1）：
//! 1. 把规则包解析成运行期结构（`LoadedPackage`）；
//! 2. 把字段引用 `{"fact": "a.b"}` **一次性**解析成 `{"slot": 3}`（编译期已经做过一次，
//!    这里做的是「加载期复核」：IR 若仍带 fact 名就补解析，若带 slot 就校验下标）；
//! 3. 按 scene 选规则集 + 按「前置事实」门控规则，避免千条全扫。
//!
//! 明确不做：不求值（B2）、不取数（A-2）、不校验业务语义（B7）。

use gr_contract::{
    Decision, ErrorCode, ErrorContract, Expr, HitStrategy, OutputSchema, PackageMetadata, Rule,
    RulePackage, RuleSet, SlotTable, TestCase,
};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

pub mod limits;

pub use limits::LoadLimits;

/// 加载失败：把契约级问题聚合成一个错误响应（`contracts/05` §3.2）。
#[derive(Debug)]
pub struct LoadError {
    pub issues: Vec<gr_contract::ContractIssue>,
}

impl LoadError {
    fn one(code: ErrorCode, detail: impl Into<String>) -> Self {
        Self { issues: vec![gr_contract::ContractIssue::new(code, detail)] }
    }

    pub fn add(&mut self, code: ErrorCode, detail: impl Into<String>) {
        self.issues.push(gr_contract::ContractIssue::new(code, detail));
    }

    pub fn into_contract(self) -> ErrorContract {
        let first = self.issues.first().cloned();
        let (code, message) = match &first {
            Some(issue) => (issue.code, issue.detail.clone()),
            None => (ErrorCode::IrInvalid, "规则包加载失败".to_string()),
        };
        let mut details = BTreeMap::new();
        details.insert(
            "issues".to_string(),
            serde_json::to_value(&self.issues).unwrap_or(serde_json::Value::Null),
        );
        details.insert("issueCount".to_string(), serde_json::json!(self.issues.len()));
        ErrorContract { error_code: code, message, trace_id: None, details }
    }
}

impl From<LoadError> for ErrorContract {
    fn from(e: LoadError) -> Self {
        e.into_contract()
    }
}

impl std::fmt::Display for LoadError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "load error: {:?}", self.issues)
    }
}

impl std::error::Error for LoadError {}

pub type LoadResult<T> = Result<T, LoadError>;

/// 加载后的一条规则。`rule` 中的字段引用已全部是 slot 下标。
#[derive(Clone, Debug)]
pub struct LoadedRule {
    pub rule: Rule,
    /// 排序后的位置（0 = 优先级最高）
    pub index: u32,
    /// 「前置事实」：`when` 中最左侧引用的 slot。
    /// 该事实缺失时这条规则一定不可能命中 → 直接跳过（剪枝）。
    pub gating_slot: Option<u32>,
    pub referenced_slots: Vec<u32>,
    /// 是否包含 assign（有输出变量）
    pub has_assign: bool,
}

#[derive(Clone, Debug)]
pub struct LoadedRuleSet {
    pub ruleset_id: String,
    pub version: String,
    pub scene: String,
    pub hit_strategy: HitStrategy,
    pub default_decision: Decision,
    pub fallback_decision: Option<Decision>,
    pub slot_table: SlotTable,
    /// 字段名 → slot（加载期建一次，运行期只碰下标）
    pub slot_index: BTreeMap<String, u32>,
    /// 按 (priority desc, ruleId asc) 排序后的规则
    pub rules: Vec<Arc<LoadedRule>>,
    /// 前置事实 → 规则下标。用于统计「本场景实际扫了多少条」与按事实批量跳过。
    pub gate_groups: BTreeMap<u32, Vec<u32>>,
    pub output_schema_id: Option<String>,
    pub output_schema_version: Option<String>,
    pub rule_count: usize,
}

impl LoadedRuleSet {
    /// 本次决策需要求值的规则下标（保持优先级顺序）。
    ///
    /// 剪枝规则：某规则的「前置事实」缺失 → 该规则必然不命中，跳过。
    /// 注意不能重排：`FIRST` 策略的语义就是「优先级序首条命中即停」，重排会改语义。
    pub fn plan(&self, missing_slots: &BTreeSet<u32>) -> Vec<u32> {
        self.rules
            .iter()
            .filter(|r| match r.gating_slot {
                Some(slot) => !missing_slots.contains(&slot),
                None => true,
            })
            .map(|r| r.index)
            .collect()
    }

    pub fn rule(&self, index: u32) -> &Arc<LoadedRule> {
        &self.rules[index as usize]
    }
}

/// 加载后的规则包（不可变，可被多线程共享）。
#[derive(Clone, Debug)]
pub struct LoadedPackage {
    pub package_id: String,
    pub package_version: String,
    pub fact_schema_version: String,
    pub dsl_version: String,
    pub ir_version: String,
    pub signature: Option<String>,
    pub metadata: PackageMetadata,
    pub rulesets: BTreeMap<String, Arc<LoadedRuleSet>>,
    /// scene → 该场景挂载的 ruleset（顺序稳定）
    pub scene_index: BTreeMap<String, Vec<String>>,
    pub tests: Vec<TestCase>,
    pub output_schemas: BTreeMap<(String, String), OutputSchema>,
    pub rule_count: usize,
}

impl LoadedPackage {
    pub fn ruleset(&self, id: &str) -> Option<&Arc<LoadedRuleSet>> {
        self.rulesets.get(id)
    }

    /// 场景下的规则集（顺序 = 该包里出现顺序）。单次决策只在这里面的规则集里选一个。
    pub fn scene_rulesets(&self, scene: &str) -> Vec<&Arc<LoadedRuleSet>> {
        self.scene_index
            .get(scene)
            .map(|ids| ids.iter().filter_map(|id| self.rulesets.get(id)).collect())
            .unwrap_or_default()
    }

    /// 场景 + 规则集版本，或场景下唯一规则集。用于 `decide` 的目标选择。
    pub fn select_ruleset(
        &self,
        scene: &str,
        ruleset_id: Option<&str>,
        ruleset_version: Option<&str>,
    ) -> LoadResult<&Arc<LoadedRuleSet>> {
        if let Some(id) = ruleset_id {
            let rs = self.rulesets.get(id).ok_or_else(|| {
                LoadError::one(
                    ErrorCode::VersionRulesetNotFound,
                    format!("规则包 {} 中没有规则集 {}", self.package_id, id),
                )
            })?;
            if let Some(v) = ruleset_version {
                if rs.version != v {
                    return Err(LoadError::one(
                        ErrorCode::VersionRulesetNotFound,
                        format!("规则集 {id} 的版本是 {}，请求 {}", rs.version, v),
                    ));
                }
            }
            return Ok(rs);
        }

        let candidates = self.scene_rulesets(scene);
        if candidates.is_empty() {
            return Err(LoadError::one(
                ErrorCode::VersionRulesetNotFound,
                format!("场景 {scene} 没有挂载任何规则集"),
            ));
        }
        if let Some(v) = ruleset_version {
            let matched: Vec<_> = candidates.iter().filter(|r| r.version == v).collect();
            return match matched.len() {
                1 => Ok(matched[0]),
                0 => Err(LoadError::one(
                    ErrorCode::VersionRulesetNotFound,
                    format!("场景 {scene} 下没有版本为 {v} 的规则集"),
                )),
                _ => Err(LoadError::one(
                    ErrorCode::VersionSelectionConflict,
                    format!("场景 {scene} 下版本 {v} 对应多个规则集，请显式指定 rulesetId"),
                )),
            };
        }
        if candidates.len() > 1 {
            return Err(LoadError::one(
                ErrorCode::VersionSelectionConflict,
                format!(
                    "场景 {scene} 挂载了 {} 个规则集，需显式指定 rulesetId（P0 不做多规则集编排）",
                    candidates.len()
                ),
            ));
        }
        Ok(candidates[0])
    }
}

/// 加载器：`RulePackage`（JSON 结构）→ `LoadedPackage`（运行期结构）。
pub fn load(pkg: &RulePackage, limits: &LoadLimits) -> LoadResult<LoadedPackage> {
    let mut errors = LoadError { issues: Vec::new() };

    if pkg.ir_version.split('.').next() != Some("1") {
        errors.add(
            ErrorCode::IrVersionUnsupported,
            format!("不支持的 ir-version: {}（本引擎支持 1.x）", pkg.ir_version),
        );
    }
    if pkg.rulesets.is_empty() {
        errors.add(ErrorCode::IrInvalid, "规则包内没有任何规则集");
    }
    if pkg.rule_count() > limits.max_rules_per_package {
        errors.add(
            ErrorCode::ExecRuleLimitExceeded,
            format!(
                "规则数 {} 超过单包上限 {}",
                pkg.rule_count(),
                limits.max_rules_per_package
            ),
        );
    }
    if !errors.issues.is_empty() {
        return Err(errors);
    }

    let mut rulesets = BTreeMap::new();
    let mut scene_index: BTreeMap<String, Vec<String>> = BTreeMap::new();
    let mut rule_count = 0usize;

    for rs in &pkg.rulesets {
        let loaded = load_ruleset(rs, pkg, limits, &mut errors)?;
        rule_count += loaded.rule_count;
        scene_index
            .entry(loaded.scene.clone())
            .or_default()
            .push(loaded.ruleset_id.clone());
        rulesets.insert(loaded.ruleset_id.clone(), Arc::new(loaded));
    }

    if !errors.issues.is_empty() {
        return Err(errors);
    }

    let output_schemas = pkg
        .output_schemas
        .iter()
        .map(|s| ((s.output_schema_id.clone(), s.output_schema_version.clone()), s.clone()))
        .collect();

    Ok(LoadedPackage {
        package_id: pkg.package_id.clone(),
        package_version: pkg.package_version.clone(),
        fact_schema_version: pkg.fact_schema_version.clone(),
        dsl_version: pkg.dsl_version.clone(),
        ir_version: pkg.ir_version.clone(),
        signature: pkg.signature.clone(),
        metadata: pkg.metadata.clone(),
        rulesets,
        scene_index,
        tests: pkg.tests.clone(),
        output_schemas,
        rule_count,
    })
}

fn load_ruleset(
    rs: &RuleSet,
    pkg: &RulePackage,
    limits: &LoadLimits,
    errors: &mut LoadError,
) -> LoadResult<LoadedRuleSet> {
    if rs.rules.len() > limits.max_rules_per_ruleset {
        errors.add(
            ErrorCode::ExecRuleLimitExceeded,
            format!(
                "规则集 {} 规则数 {} 超过上限 {}",
                rs.ruleset_id,
                rs.rules.len(),
                limits.max_rules_per_ruleset
            ),
        );
    }

    let slot_table: SlotTable =
        if rs.slot_table.is_empty() { pkg.slot_table.clone() } else { rs.slot_table.clone() };

    let mut slot_index: BTreeMap<String, u32> = BTreeMap::new();
    for (idx, slot) in slot_table.iter().enumerate() {
        if let Some(prev) = slot_index.insert(slot.name.clone(), idx as u32) {
            errors.add(
                ErrorCode::IrDuplicateSlot,
                format!(
                    "规则集 {} 的 slot 表里 {} 重复（下标 {} 与 {}）",
                    rs.ruleset_id, slot.name, prev, idx
                ),
            );
        }
    }

    let mut loaded_rules: Vec<LoadedRule> = Vec::with_capacity(rs.rules.len());
    for rule in &rs.rules {
        let mut when = rule.when.clone();
        resolve_expr(&mut when, &slot_index, &slot_table, &rs.ruleset_id, &rule.rule_id, errors);

        let mut then = rule.then.clone();
        for assign in &mut then.assign {
            resolve_expr(
                &mut assign.expr,
                &slot_index,
                &slot_table,
                &rs.ruleset_id,
                &rule.rule_id,
                errors,
            );
        }

        if when.depth() > limits.max_expr_depth {
            errors.add(
                ErrorCode::DslDepthLimit,
                format!(
                    "{}/{} 表达式深度 {} 超过上限 {}",
                    rs.ruleset_id,
                    rule.rule_id,
                    when.depth(),
                    limits.max_expr_depth
                ),
            );
        }
        if when.node_count() > limits.max_expr_nodes {
            errors.add(
                ErrorCode::DslNodeLimit,
                format!(
                    "{}/{} 表达式节点数 {} 超过上限 {}",
                    rs.ruleset_id,
                    rule.rule_id,
                    when.node_count(),
                    limits.max_expr_nodes
                ),
            );
        }

        let mut slots = Vec::new();
        when.collect_slots(&mut slots);
        for assign in &then.assign {
            assign.expr.collect_slots(&mut slots);
        }
        slots.sort_unstable();
        slots.dedup();

        let gating_slot = first_slot(&when);
        let has_assign = !then.assign.is_empty();

        loaded_rules.push(LoadedRule {
            rule: Rule { when, then, ..rule.clone() },
            index: 0,
            gating_slot,
            referenced_slots: slots,
            has_assign,
        });
    }

    // 排序：priority 降序，同优先级按 ruleId 升序（`contracts/05` §8.2，保证可复现）
    loaded_rules.sort_by(|a, b| {
        b.rule
            .priority
            .cmp(&a.rule.priority)
            .then_with(|| a.rule.rule_id.cmp(&b.rule.rule_id))
    });

    let mut gate_groups: BTreeMap<u32, Vec<u32>> = BTreeMap::new();
    for (idx, rule) in loaded_rules.iter_mut().enumerate() {
        rule.index = idx as u32;
        if let Some(gate) = rule.gating_slot {
            gate_groups.entry(gate).or_default().push(idx as u32);
        }
    }

    let rule_count = loaded_rules.len();

    if !errors.issues.is_empty() {
        return Err(LoadError { issues: std::mem::take(&mut errors.issues) });
    }

    Ok(LoadedRuleSet {
        ruleset_id: rs.ruleset_id.clone(),
        version: rs.version.clone().unwrap_or_else(|| "unversioned".to_string()),
        scene: rs.scene.clone(),
        hit_strategy: rs.hit_strategy,
        default_decision: rs.default_decision.clone(),
        fallback_decision: rs.fallback_decision.clone(),
        slot_table,
        slot_index,
        rules: loaded_rules.into_iter().map(Arc::new).collect(),
        gate_groups,
        output_schema_id: rs.output_schema_id.clone(),
        output_schema_version: rs.output_schema_version.clone(),
        rule_count,
    })
}

/// `Fact` → `Slot`；`Slot` 校验下标；其余递归。
fn resolve_expr(
    expr: &mut Expr,
    slot_index: &BTreeMap<String, u32>,
    slot_table: &SlotTable,
    ruleset_id: &str,
    rule_id: &str,
    errors: &mut LoadError,
) {
    match expr {
        Expr::Fact(f) => match slot_index.get(&f.fact) {
            Some(idx) => *expr = Expr::Slot(gr_contract::SlotRef { slot: *idx }),
            None => errors.add(
                ErrorCode::IrUnknownFact,
                format!(
                    "{ruleset_id}/{rule_id} 引用的字段 {} 不在 slot 表中（共 {} 个字段）",
                    f.fact,
                    slot_table.len()
                ),
            ),
        },
        Expr::Slot(s) => {
            if s.slot as usize >= slot_table.len() {
                errors.add(
                    ErrorCode::IrUnknownSlot,
                    format!(
                        "{ruleset_id}/{rule_id} 引用的 slot {} 越界（slot 表长度 {}）",
                        s.slot,
                        slot_table.len()
                    ),
                );
            }
        }
        Expr::Op(o) => {
            for arg in &mut o.args {
                resolve_expr(arg, slot_index, slot_table, ruleset_id, rule_id, errors);
            }
        }
        Expr::Literal(_) => {}
    }
}

/// 最左侧引用的 slot（求值顺序上的第一个事实）= 前置事实。
fn first_slot(expr: &Expr) -> Option<u32> {
    match expr {
        Expr::Slot(s) => Some(s.slot),
        Expr::Op(o) => o.args.iter().find_map(first_slot),
        _ => None,
    }
}

/// 便捷：从 JSON 文本加载。
pub fn load_from_json(raw: &str, limits: &LoadLimits) -> LoadResult<LoadedPackage> {
    let pkg: RulePackage = serde_json::from_str(raw).map_err(|e| {
        LoadError::one(ErrorCode::IrInvalid, format!("规则包 JSON 解析失败: {e}"))
    })?;
    load(&pkg, limits)
}
