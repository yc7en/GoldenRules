//! F-PKG06 acceptance: the approved test-case gate.
//!
//! The value of a gate lies entirely in "the assertion really ran": so every case covers both directions --
//! what should pass must pass (otherwise the gate would block every publish), and what should be blocked must be blocked (otherwise the gate is mere decoration).

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

/// Set the fixture's test case to the given status (the fixture itself is pending_review).
///
/// The scene mounts 3 rulesets, so the case must specify `rulesetId` -- which is exactly what the gate has to teach
/// the case author (see `ambiguous_scene_blocks_the_gate`).
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

// An approved case with the expected-correct result: the gate passes it, and `ran` shows it really executed.
#[test]
fn approved_passing_case_opens_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |_| {});
    let r = run(&pkg);
    assert!(r.ok(), "应放行: {r:?}");
    assert_eq!((r.total, r.ran, r.passed, r.failed, r.skipped), (1, 1, 1, 0, 0));
    assert!(r.summary().contains("1/1"), "{}", r.summary());
}

// A decision mismatch must block, and the reason must spell out "what was expected, what was actual".
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

// The decision is right but the reason code is wrong is blocked all the same: the reason code is what callers and complaint reviews rely on.
#[test]
fn reason_code_mismatch_blocks_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.expect.reason_code = "SOMETHING_ELSE".into();
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(failure_of(&r).code, "REASON_MISMATCH");
}

// Output field comparison: expected values follow subset semantics, whatever is written must match.
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

    // Passes when the expected value is correct (subset semantics: writing only the fields you care about is enough)
    pkg.tests[0].expect.outputs.insert(
        "riskLevel".into(),
        Value::Str("MEDIUM".into()),
    );
    assert!(run(&pkg).ok(), "正确的输出期望应放行");
}

// An expected output names a field the engine never produced -> report it as "missing" explicitly instead of silently letting it through.
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

// Unapproved cases are not run and do not block, but must be counted in `skipped` --
// "not verified" and "verified" must not look the same in the report.
#[test]
fn pending_cases_are_skipped_not_passed() {
    let pkg = with_case(TestCaseStatus::PendingReview, |tc| {
        // Deliberately wrong expectation: if it were run, it would certainly fail
        tc.expect.decision = Decision::Reject;
        tc.expect.reason_code = "NOPE".into();
    });
    let r = run(&pkg);
    assert!(r.ok(), "未批准用例不应阻断发布");
    assert_eq!((r.ran, r.passed, r.failed, r.skipped), (0, 0, 0, 1));
    assert!(r.summary().contains("跳过 1"), "{}", r.summary());
}

// Nothing is approved: pass but with ran=0, so the caller can distinguish "no cases" from "all cases passed".
#[test]
fn no_approved_cases_passes_with_zero_ran() {
    let mut pkg = package();
    pkg.tests.clear();
    let r = run(&pkg);
    assert!(r.ok());
    assert_eq!((r.total, r.ran, r.skipped), (0, 0, 0));
}

// All failures collected in one run: the publisher must see every bad case at once, not fix one and run again.
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

// The case's scene does not exist in the package -> an engine error must also be collected as one failure; it must not 500 the whole gate.
#[test]
fn unknown_scene_reports_engine_error() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.scene = "scene-that-does-not-exist".into();
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(failure_of(&r).code, "ENGINE_ERROR");
}

// The scene mounts several rulesets and the case does not say which one it targets -> reject explicitly, never pick one for it.
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

// The case names a ruleset but that ruleset is not on this scene -> also a configuration error; report it clearly instead of guessing.
#[test]
fn ruleset_mismatch_blocks_the_gate() {
    let pkg = with_case(TestCaseStatus::Approved, |tc| {
        tc.ruleset_id = Some("rs-withdraw-rate".into()); // belongs to another scene
    });
    let r = run(&pkg);
    assert!(!r.ok());
    assert_eq!(failure_of(&r).code, "RULESET_MISMATCH");
}

// When a scene mounts only one ruleset, rulesetId is unnecessary (one less line written must not block the publish).
#[test]
fn single_ruleset_scene_needs_no_ruleset_id() {
    let mut pkg = package();
    // Keep only rs-withdraw-rate (which exclusively owns the withdraw-rate scene)
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
    // What is asserted is "the ruleset was resolved automatically": it only needs to not be AMBIGUOUS_SCENE,
    // whether the business conclusion is right is not this case's job.
    if let Some(f) = r.failures.first() {
        assert_ne!(f.code, "AMBIGUOUS_SCENE", "单规则集场景不该要求 rulesetId");
    }
}

// The gate must use a **fixed time baseline**: two runs give identical results, otherwise time-conditional rules flicker red/green.
#[test]
fn verification_is_deterministic() {
    let pkg = with_case(TestCaseStatus::Approved, |_| {});
    let a = run(&pkg);
    let b = run(&pkg);
    assert_eq!(a, b);
}

// When the package itself does not compile, verify returns Err (not "case failure"): the two situations have different owners.
#[test]
fn uncompilable_package_is_an_error_not_a_failure() {
    let mut pkg = with_case(TestCaseStatus::Approved, |_| {});
    pkg.rulesets[0].rules[0].when =
        serde_json::from_value(json!({"op": "no_such_op", "args": []})).expect("expr");
    let err = verify_package(&pkg, &catalog(), EngineConfig::default());
    assert!(err.is_err(), "坏包应返回 Err: {err:?}");
}
