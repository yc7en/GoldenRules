//! approved 用例发布门禁（F-PKG06 / `contracts/06` §5）。
//!
//! 规则包发布前，把包内**状态为 approved 的用例**真跑一遍：任一失败就阻断发布。
//!
//! 为什么门禁必须跑真引擎而不是比对 IR 或做静态检查：
//! 用户故事的验收标准是「这笔提款应该被拒」，只有走完整条 快照→VM→结果 的链路
//! 才能证明写下的规则真的会得出那个结论。静态检查只能证明「写对了形状」。
//!
//! 为什么放在引擎侧而不是平台侧：VM 只有这一份实现，平台侧再写一个「近似执行器」
//! 必然与线上行为漂移——用漂移的执行器当门禁，等于没有门禁。
//!
//! 三条明确的取舍：
//! 1. **只跑 approved**。pending_review / rejected 是草稿，不该阻断发布，
//!    也不该被当作「已验证」——所以报告里单独给出 `skipped`，不混进 `passed`。
//! 2. **确定性时间**。用例没有「当前时间」概念，统一用固定 `requestTime`，
//!    否则含时间条件的规则会跑出随机结果（A-5：引擎不读系统时钟）。
//! 3. **失败即全量收集**，不在第一个失败处短路：发布人需要一次看到所有坏用例，
//!    而不是修一个跑一次。

use gr_contract::{
    Decision, ErrorContract, ExecutionContext, FactCatalog, FactSnapshot, RulePackage, TestCase,
    TestCaseStatus, Value,
};
use std::collections::BTreeMap;

use crate::{Engine, EngineConfig};

/// 门禁执行时使用的固定时间基准。
pub const VERIFY_REQUEST_TIME: &str = "1970-01-01T00:00:00Z";
/// 门禁用例使用的租户（避免与线上租户数据混淆，也让日志一眼可辨）。
pub const VERIFY_TENANT: &str = "pkg-verify";

/// 单个用例的失败原因。
#[derive(Clone, Debug, PartialEq)]
pub struct TestFailure {
    pub test_id: String,
    /// `DECISION_MISMATCH` / `REASON_MISMATCH` / `OUTPUT_MISMATCH` / `ENGINE_ERROR`
    pub code: String,
    pub message: String,
}

/// 门禁报告。
#[derive(Clone, Debug, Default, PartialEq)]
pub struct VerifyReport {
    /// 包内用例总数
    pub total: usize,
    /// 状态为 approved、被真跑过的用例数
    pub ran: usize,
    pub passed: usize,
    pub failed: usize,
    /// 非 approved 而未执行的用例数
    pub skipped: usize,
    pub failures: Vec<TestFailure>,
}

impl VerifyReport {
    /// 门禁是否放行：没有失败即放行（`ran == 0` 也放行——「没有已批准用例」
    /// 不等于「用例没通过」，这两种情况必须在报告里分得清）。
    pub fn ok(&self) -> bool {
        self.failed == 0
    }

    /// 一行摘要，便于写进发布记录与审计。
    pub fn summary(&self) -> String {
        format!(
            "approved 用例 {}/{} 通过（跳过 {} 条未批准用例）",
            self.passed, self.ran, self.skipped
        )
    }
}

/// 跑包内 approved 用例。
///
/// 编译失败直接以 `Err` 返回（包本身不成立，谈不上「用例通过率」）；
/// 用例失败收进 `report.failures`，由调用方决定阻断。
pub fn verify_package(
    pkg: &RulePackage,
    catalog: &FactCatalog,
    config: EngineConfig,
) -> Result<VerifyReport, ErrorContract> {
    let mut report = VerifyReport {
        total: pkg.tests.len(),
        ..Default::default()
    };
    let approved: Vec<&TestCase> = pkg
        .tests
        .iter()
        .filter(|t| t.status == Some(TestCaseStatus::Approved))
        .collect();
    report.skipped = report.total - approved.len();
    report.ran = approved.len();
    if approved.is_empty() {
        return Ok(report);
    }

    // 单包引擎：用发布候选包自身编译，跑的就是即将上线的那份产物。
    let engine = Engine::new(catalog.clone(), vec![pkg.clone()], config)?;

    for tc in approved {
        match run_case(&engine, pkg, tc) {
            Ok(()) => report.passed += 1,
            Err(f) => {
                report.failed += 1;
                report.failures.push(f);
            }
        }
    }
    Ok(report)
}

/// 跑单条用例：先解析规则集 → 构造快照 → decide → 比对 决策/原因码/输出。
fn run_case(engine: &Engine, pkg: &RulePackage, tc: &TestCase) -> Result<(), TestFailure> {
    let ruleset_id = resolve_ruleset(pkg, tc)?;

    let snapshot = FactSnapshot {
        facts: tc.input.clone(),
        ..FactSnapshot::new(
            format!("verify-{}", tc.test_id),
            VERIFY_TENANT,
            tc.scene.clone(),
            pkg.fact_schema_version.clone(),
        )
    };
    let mut ctx = ExecutionContext::new(VERIFY_TENANT, tc.scene.clone(), VERIFY_REQUEST_TIME);
    ctx.ruleset_id = ruleset_id;

    let out = engine.decide(&snapshot, &ctx).map_err(|e| TestFailure {
        test_id: tc.test_id.clone(),
        code: "ENGINE_ERROR".to_string(),
        message: format!("决策执行失败：{} {}", e.error_code, e.message),
    })?;

    let r = &out.result;
    if r.decision != tc.expect.decision {
        return Err(TestFailure {
            test_id: tc.test_id.clone(),
            code: "DECISION_MISMATCH".to_string(),
            message: format!(
                "期望 {} 实际 {}（原因码 {}）",
                decision_text(&tc.expect.decision),
                decision_text(&r.decision),
                r.reason_code
            ),
        });
    }
    if r.reason_code != tc.expect.reason_code {
        return Err(TestFailure {
            test_id: tc.test_id.clone(),
            code: "REASON_MISMATCH".to_string(),
            message: format!(
                "期望原因码 {} 实际 {}",
                tc.expect.reason_code, r.reason_code
            ),
        });
    }
    // 期望输出是**子集**语义：用例只关心它写出来的那几个字段，
    // 规则新增的输出字段不该让老用例失败。
    let mut bad: Vec<String> = Vec::new();
    for (k, want) in &tc.expect.outputs {
        match r.outputs.get(k) {
            None => bad.push(format!("{k} 缺失（期望 {}）", value_text(want))),
            Some(got) if got != want => {
                bad.push(format!("{k} 期望 {} 实际 {}", value_text(want), value_text(got)))
            }
            Some(_) => {}
        }
    }
    if !bad.is_empty() {
        return Err(TestFailure {
            test_id: tc.test_id.clone(),
            code: "OUTPUT_MISMATCH".to_string(),
            message: bad.join("；"),
        });
    }
    Ok(())
}

/// 解析用例针对哪个规则集。
///
/// 规则：场景只挂一个规则集 → 就是它；挂多个 → 用例必须写明 `rulesetId`。
///
/// 为什么不在多挂时「挑一个跑」：P0 不做多规则集编排，运行期遇到这种场景是直接报
/// `VERSION_SELECTION_CONFLICT` 的。门禁若替它挑一个跑通，就会给出一个线上永远不会
/// 出现的「通过」——比不放行更危险。
fn resolve_ruleset(pkg: &RulePackage, tc: &TestCase) -> Result<Option<String>, TestFailure> {
    let mounted: Vec<&str> = pkg
        .rulesets
        .iter()
        .filter(|rs| rs.scene == tc.scene)
        .map(|rs| rs.ruleset_id.as_str())
        .collect();

    match mounted.len() {
        0 => {
            // 交给引擎报错：错误码与线上一致，便于对上日志。
            Ok(tc.ruleset_id.clone())
        }
        1 => Ok(Some(mounted[0].to_string())),
        _ => match &tc.ruleset_id {
            Some(id) if mounted.contains(&id.as_str()) => Ok(Some(id.clone())),
            Some(id) => Err(TestFailure {
                test_id: tc.test_id.clone(),
                code: "RULESET_MISMATCH".to_string(),
                message: format!(
                    "用例指定的规则集 {id} 不在场景 {} 上（该场景挂载：{}）",
                    tc.scene,
                    mounted.join(", ")
                ),
            }),
            None => Err(TestFailure {
                test_id: tc.test_id.clone(),
                code: "AMBIGUOUS_SCENE".to_string(),
                message: format!(
                    "场景 {} 挂载了 {} 个规则集（{}），用例必须写明 rulesetId",
                    tc.scene,
                    mounted.len(),
                    mounted.join(", ")
                ),
            }),
        },
    }
}

fn decision_text(d: &Decision) -> String {
    d.as_str().to_string()
}

fn value_text(v: &Value) -> String {
    serde_json::to_value(v)
        .map(|j| j.to_string())
        .unwrap_or_else(|_| "<unserializable>".to_string())
}

/// 把用例输入转成快照字段（供调用方做「用例输入是否覆盖了必填事实」这类前置检查）。
///
/// 单独抽出来是因为它是唯一一处「用例字段名 = 快照字段名」的假设，
/// 将来若引入用例入参映射，只需要改这里。
pub fn case_facts(tc: &TestCase) -> BTreeMap<String, serde_json::Value> {
    tc.input.clone()
}
