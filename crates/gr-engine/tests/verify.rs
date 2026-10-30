//! F-PKG06 验收：approved 用例门禁。
//!
//! 门禁的价值全在「断言真的跑起来了」：所以每条用例都同时覆盖两个方向——
//! 该过的必须过（否则门禁会把发布全堵死），该拦的必须拦（否则门禁等于装饰）。

use gr_contract::*;
use gr_engine::verify::{verify_package, TestFailure, VerifyReport};
use gr_engine::EngineConfig;
use serde_json::json;

fn fixture(name: &str) -> String {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

fn catalog() -> FactCatalog {
    serde_json::from_str(&fixture("fact-catalog.json")).expect("catalog")
}

fn package() -> RulePackage {
    serde_json::from_str(&fixture("rule-package.json")).expect("package")
}

/// 把 fixture 里那条用例改成指定状态（fixture 本身是 pending_review）。
///
/// 该场景挂载了 3 个规则集，所以用例必须写明 `rulesetId`——这也正是门禁要教给
/// 用例作者的事（见 `ambiguous_scene_blocks_the_gate`）。
fn with_case(status: TestCaseStatus, mutate: impl FnOnce(&mut TestCase)) -> RulePackage {
    let mut pkg = package();
    let mut tc = pkg.tests[0].clone();
    tc.status = Some(status);
    tc.ruleset_id = Some("rs-amount-limit".into());
    mutate(&mut tc);
    pkg.tests = vec![tc];
    pkg
}

fn run(pkg: &RulePackage) -> VerifyReport {
    verify_package(pkg, &catalog(), EngineConfig::default()).expect("verify")
}

fn failure_of(r: &VerifyReport) -> &TestFailure {
    r.failures.first().expect("应有一条失败")
}

// 已批准且预期正确的用例：门禁放行，且 `ran` 显示真的执行了。
#[test]
fn approved_passing_case_opens_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |_| {});
    let r = run(&pkg);
    assert!(r.ok(), "应放行: {r:?}");
    assert_eq!((r.total, r.ran, r.passed, r.failed, r.skipped), (1, 1, 1, 0, 0));
    assert!(r.summary().contains("1/1"), "{}", r.summary());
}

// 决策不一致必须拦下，并且原因要说清「期望什么、实际什么」。
#[test]
fn decision_mismatch_blocks_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.expect.decision = Decision::Reject;
        tc.expect.reason_code = "AMOUNT_LIMIT_EXCEEDED".into();
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(r.failed, 1);
    let f = failure_of(&r);
    assert_eq!(f.code, "DECISION_MISMATCH");
    assert!(f.message.contains("REJECT") && f.message.contains("REVIEW"), "{}", f.message);
}

// 决策对了但原因码不对同样拦：原因码是调用方与客诉复盘的依据。
#[test]
fn reason_code_mismatch_blocks_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.expect.reason_code = "SOMETHING_ELSE".into();
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(failure_of(&r).code, "REASON_MISMATCH");
}

// 输出字段比对：期望值是子集语义，写了的必须对上。
#[test]
fn output_mismatch_blocks_the_gate() {
    let mut pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.expect.outputs
            .insert("riskLevel".into(), Value::Str("HIGH".into()));
    });
    let r = run(&pkg);
    assert!(!r.ok(), "输出不符应拦下");
    let f = failure_of(&r);
    assert_eq!(f.code, "OUTPUT_MISMATCH");
    assert!(f.message.contains("riskLevel"), "{}", f.message);

    // 期望值正确时放行（子集语义：只写想校验的字段就够）
    pkg.tests[0].expect.outputs.insert(
        "riskLevel".into(),
        Value::Str("MEDIUM".into()),
    );
    assert!(run(&pkg).ok(), "正确的输出期望应放行");
}

// 期望输出里写了引擎根本没产出的字段 → 明确报「缺失」，而不是静默放过。
#[test]
fn missing_expected_output_is_a_failure() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.expect
            .outputs
            .insert("notProduced".into(), Value::Str("x".into()));
    });
    let r = run(&pkg);
    assert!(!r.ok());
    let f = failure_of(&r);
    assert!(f.message.contains("notProduced") && f.message.contains("缺失"), "{}", f.message);
}

// 未批准的用例不跑、不阻断，但必须记在 `skipped` 里——
// 「没验」和「验过了」在报告里不能长得一样。
#[test]
fn pending_cases_are_skipped_not_passed() {
    let pkg = with_case(TestCaseStatus::PendingReview, |tc| {
        // 故意写错预期：如果它被跑了，一定失败
        tc.expect.decision = Decision::Reject;
        tc.expect.reason_code = "NOPE".into();
    });
    let r = run(&pkg);
    assert!(r.ok(), "未批准用例不应阻断发布");
    assert_eq!((r.ran, r.passed, r.failed, r.skipped), (0, 0, 0, 1));
    assert!(r.summary().contains("跳过 1"), "{}", r.summary());
}

// 一个 approved 都没有：放行但 ran=0，让调用方能区分「没用例」与「用例全过」。
#[test]
fn no_approved_cases_passes_with_zero_ran() {
    let mut pkg = package();
    pkg.tests.clear();
    let r = run(&pkg);
    assert!(r.ok());
    assert_eq!((r.total, r.ran, r.skipped), (0, 0, 0));
}

// 多条用例一次全收集：发布人要一次看到所有坏用例，而不是修一个跑一次。
#[test]
fn all_failures_are_collected() {
    let mut pkg = package();
    let mut a = pkg.tests[0].clone();
    a.test_id = "T-BAD-1".into();
    a.status = Some(TestCaseStatus::Approved);
    a.expect.decision = Decision::Reject;
    let mut b = a.clone();
    b.test_id = "T-BAD-2".into();
    b.expect.reason_code = "X".into();
    pkg.tests = vec![a, b];
    let r = run(&pkg);
    assert_eq!(r.failed, 2);
    let ids: Vec<&str> = r.failures.iter().map(|f| f.test_id.as_str()).collect();
    assert_eq!(ids, vec!["T-BAD-1", "T-BAD-2"]);
}

// 用例的 scene 在包里不存在 → 引擎报错也要收成一条失败，不能让整个门禁 500。
#[test]
fn unknown_scene_reports_engine_error() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.scene = "scene-that-does-not-exist".into();
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(failure_of(&r).code, "ENGINE_ERROR");
}

// 场景挂多个规则集而用例没写明针对哪个 → 明确拒绝，绝不替它挑一个。
#[test]
fn ambiguous_scene_blocks_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.ruleset_id = None;
    });
    let r = run(&pkg);
    assert!(!r.ok());
    let f = failure_of(&r);
    assert_eq!(f.code, "AMBIGUOUS_SCENE");
    assert!(
        f.message.contains("rs-amount-limit") && f.message.contains("rulesetId"),
        "诊断应指出候选规则集与修法：{}",
        f.message
    );
}

// 写明了规则集但该规则集不在这个场景上 → 也是配置错误，报清楚而不是猜。
#[test]
fn ruleset_mismatch_blocks_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.ruleset_id = Some("rs-withdraw-rate".into()); // 属于另一个场景
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(failure_of(&r).code, "RULESET_MISMATCH");
}

// 场景只挂一个规则集时无需写明 rulesetId（少写一句不该拦住发布）。
#[test]
fn single_ruleset_scene_needs_no_ruleset_id() {
    let mut pkg = package();
    // 只保留 rs-withdraw-rate（独占 withdraw-rate 场景）
    pkg.rulesets.retain(|rs| rs.ruleset_id == "rs-withdraw-rate");
    let mut tc = pkg.tests[0].clone();
    tc.test_id = "T-RATE".into();
    tc.scene = "withdraw-rate".into();
    tc.ruleset_id = None;
    tc.status = Some(TestCaseStatus::Approved);
    tc.input.clear();
    tc.expect.decision = Decision::Approve;
    tc.expect.reason_code = String::new();
    tc.expect.outputs.clear();
    pkg.tests = vec![tc];
    let r = run(&pkg);
    // 断言的是「规则集被自动解析出来了」：只要不是 AMBIGUOUS_SCENE 即可，
    // 业务结论对不对不是这条用例的职责。
    if let Some(f) = r.failures.first() {
        assert_ne!(f.code, "AMBIGUOUS_SCENE", "单规则集场景不该要求 rulesetId");
    }
}

// 门禁必须用**固定时间基准**：跑两次结果一致，否则含时间条件的规则会随机红绿。
#[test]
fn verification_is_deterministic() {
    let pkg = with_case(TestCaseStatus::Approved, |_| {});
    let a = run(&pkg);
    let b = run(&pkg);
    assert_eq!(a, b);
}

// 包本身编不过时 verify 返回 Err（不是「用例失败」）：这两种情况的处理人不同。
#[test]
fn uncompilable_package_is_an_error_not_a_failure() {
    let mut pkg = with_case(TestCaseStatus::Approved, |_| {});
    pkg.rulesets[0].rules[0].when =
        serde_json::from_value(json!({"op": "no_such_op", "args": []})).expect("expr");
    let err = verify_package(&pkg, &catalog(), EngineConfig::default());
    assert!(err.is_err(), "坏包应返回 Err: {err:?}");
}
