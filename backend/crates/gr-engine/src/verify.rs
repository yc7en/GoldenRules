//! approved test-case publish gate (F-PKG06 / `contracts/06` §5).
//!
//! Before a rule package is published, actually run the **test cases whose status is approved**: any failure blocks the publish.
//!
//! Why the gate must run the real engine rather than compare IR or do static checks:
//! the acceptance criterion of a user story is "this withdrawal should be rejected", and only walking the full snapshot->VM->result chain
//! can prove that the rule as written really reaches that conclusion. Static checks can only prove "the shape was written correctly".
//!
//! Why it belongs on the engine side rather than the platform side: there is only one VM implementation; a second "approximate executor" on the platform side
//! would inevitably drift from production behavior -- gating with a drifting executor is the same as having no gate.
//!
//! Three explicit trade-offs:
//! 1. **Only run approved**. pending_review / rejected are drafts: they must not block the publish,
//!    and must not be counted as "verified" either -- so the report lists `skipped` separately instead of folding it into `passed`.
//! 2. **Deterministic time**. Test cases have no notion of "current time", so a fixed `requestTime` is used uniformly;
//!    otherwise time-conditional rules would produce random results (A-5: the engine does not read the system clock).
//! 3. **Collect everything on failure**, no short-circuit at the first failure: the publisher needs to see all bad cases at once,
//!    rather than fixing one and running again.

use gr_contract::{
    Decision, ErrorContract, ExecutionContext, FactCatalog, FactSnapshot, RulePackage, TestCase,
    TestCaseStatus, Value,
};
use std::collections::BTreeMap;

use crate::{Engine, EngineConfig};

/// Fixed time baseline used when running the gate.
pub const VERIFY_REQUEST_TIME: &str = "1970-01-01T00:00:00Z";
/// Tenant used by gate test cases (keeps them from being confused with production tenant data, and makes them easy to spot in the log).
pub const VERIFY_TENANT: &str = "pkg-verify";

/// Reason a single test case failed.
#[derive(Clone, Debug, PartialEq)]
pub struct TestFailure {
    pub test_id: String,
    /// `DECISION_MISMATCH` / `REASON_MISMATCH` / `OUTPUT_MISMATCH` / `ENGINE_ERROR`
    pub code: String,
    pub message: String,
}

/// Gate report.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct VerifyReport {
    /// Total number of test cases in the package
    pub total: usize,
    /// Number of approved cases that were actually executed
    pub ran: usize,
    pub passed: usize,
    pub failed: usize,
    /// Number of cases not executed because they are not approved
    pub skipped: usize,
    pub failures: Vec<TestFailure>,
}

impl VerifyReport {
    /// Whether the gate lets the package through: no failure means pass (`ran == 0` also passes -- "there are no approved cases"
    /// is not the same as "the cases failed", and the report must keep those two apart).
    pub fn ok(&self) -> bool {
        self.failed == 0
    }

    /// One-line summary, convenient for the publish record and the audit trail.
    pub fn summary(&self) -> String {
        format!(
            "approved 用例 {}/{} 通过（跳过 {} 条未批准用例）",
            self.passed, self.ran, self.skipped
        )
    }
}

/// Run the approved test cases in the package.
///
/// A compile failure is returned directly as `Err` (the package itself does not hold up, so "case pass rate" is moot);
/// case failures are collected into `report.failures`, and the caller decides whether to block.
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

    // Single-package engine: compile from the release-candidate package itself, so what runs is exactly the artifact about to go live.
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

/// Run a single test case: resolve the ruleset -> build the snapshot -> decide -> compare decision / reason code / outputs.
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
    // Expected outputs use **subset** semantics: a case only cares about the fields it spells out,
    // so output fields added by later rules must not break older cases.
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

/// Resolve which ruleset the test case targets.
///
/// Rule: if the scene mounts exactly one ruleset -> that is the one; if it mounts several -> the case must specify `rulesetId`.
///
/// Why not "pick one and run it" when several are mounted: P0 does not do multi-ruleset orchestration, and at runtime such a scene is an
/// immediate `VERSION_SELECTION_CONFLICT`. If the gate picked one and passed it, it would report a "pass" that can never
/// occur in production -- more dangerous than blocking.
fn resolve_ruleset(pkg: &RulePackage, tc: &TestCase) -> Result<Option<String>, TestFailure> {
    let mounted: Vec<&str> = pkg
        .rulesets
        .iter()
        .filter(|rs| rs.scene == tc.scene)
        .map(|rs| rs.ruleset_id.as_str())
        .collect();

    match mounted.len() {
        0 => {
            // Let the engine raise the error: the error code matches production, which makes log correlation easier.
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

/// Turn the test-case input into snapshot facts (for callers doing upfront checks such as "does the case input cover the required facts").
///
/// Extracted separately because it is the only place assuming "test-case field name = snapshot field name";
/// if case input mapping is ever introduced, only this function needs to change.
pub fn case_facts(tc: &TestCase) -> BTreeMap<String, serde_json::Value> {
    tc.input.clone()
}
