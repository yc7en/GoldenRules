//! B5 acceptance: Providers can be self-tested in isolation + Assemble mode takes business inputs, assembles a snapshot and produces a decision.

use gr_contract::{
    DegradeReason, ErrorCode, ExecutionContext, FactCatalog, FactKind, MissingReason,
};
use gr_engine::{engine_from_json, EngineConfig};
use gr_provider::{
    Assembler, AssemblerSpec, ConstantProvider, DerivedProvider, FactSample, HttpProvider,
    Provider, ProviderCtx, StdTransport, Transport, TransportError,
};
use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

// ---------------------------------------------------------------- test scaffolding

fn fixtures() -> std::path::PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../fixtures")
}

fn read(name: &str) -> String {
    std::fs::read_to_string(fixtures().join(name))
        .unwrap_or_else(|e| panic!("读 {name} 失败: {e}"))
}

fn catalog() -> FactCatalog {
    serde_json::from_str(&read("fact-catalog.json")).expect("事实目录应可解析")
}

fn ctx(scene: &str) -> ExecutionContext {
    ExecutionContext::new("pay-cn", scene, "2026-10-30T10:00:00Z")
}

fn inputs(pairs: &[(&str, serde_json::Value)]) -> BTreeMap<String, serde_json::Value> {
    pairs.iter().map(|(k, v)| (k.to_string(), v.clone())).collect()
}

/// Controllable transport: covers the timeout / 5xx / bad-body paths with no network at all.
struct MockTransport {
    body: Result<String, TransportError>,
    calls: AtomicUsize,
    last_url: std::sync::Mutex<String>,
}

impl MockTransport {
    fn ok(body: &str) -> Arc<Self> {
        Arc::new(Self {
            body: Ok(body.to_string()),
            calls: AtomicUsize::new(0),
            last_url: std::sync::Mutex::new(String::new()),
        })
    }

    fn err(e: TransportError) -> Arc<Self> {
        Arc::new(Self { body: Err(e), calls: AtomicUsize::new(0), last_url: std::sync::Mutex::new(String::new()) })
    }
}

impl Transport for MockTransport {
    fn get(&self, url: &str, _timeout: Duration) -> Result<String, TransportError> {
        self.calls.fetch_add(1, Ordering::Relaxed);
        *self.last_url.lock().unwrap() = url.to_string();
        self.body.clone()
    }
}

fn spec(json: &str) -> AssemblerSpec {
    serde_json::from_str(json).expect("Provider 配置应可解析")
}

// ---------------------------------------------------------------- static constants

#[test]
fn constant_provider_supplies_all_declared_facts() {
    let p = ConstantProvider::new(
        "product-config",
        inputs(&[("config.withdrawDailyLimit", serde_json::json!("100000"))]),
    );
    let empty = BTreeMap::new();
    let view = ProviderCtx {
        tenant_id: "pay-cn",
        scene: "withdraw-risk-check",
        request_time: "2026-10-30T10:00:00Z",
        inputs: &empty,
        facts: &empty,
    };
    let out = p.fetch(&view);
    assert_eq!(out.len(), 1);
    assert_eq!(out[0].0, "config.withdrawDailyLimit");
    assert_eq!(
        out[0].1,
        FactSample::Value { value: serde_json::json!("100000"), meta: None }
    );
    assert_eq!(p.provides(), ["config.withdrawDailyLimit".to_string()]);
}

// ---------------------------------------------------------------- derived expressions

#[test]
fn derived_provider_computes_from_dependencies() {
    // riskScore = min(1000, max(0, count*10 + 500)): reuses the VM whitelisted operators instead of writing separate arithmetic
    let expr: gr_contract::Expr = serde_json::from_value(serde_json::json!({
        "op": "add",
        "args": [
            { "op": "mul", "args": [{ "fact": "customer.withdrawCount24h" }, { "type": "integer", "value": 10 }] },
            { "type": "integer", "value": 500 }
        ]
    }))
    .unwrap();
    let p = DerivedProvider::new(
        "risk-score",
        vec![("customer.withdrawCount24h".to_string(), FactKind::Integer)],
        BTreeMap::from([("customer.riskScore".to_string(), expr)]),
    )
    .expect("派生 Provider 应可构建");

    let facts = inputs(&[("customer.withdrawCount24h", serde_json::json!(2))]);
    let empty = BTreeMap::new();
    let out = p.fetch(&ProviderCtx {
        tenant_id: "pay-cn",
        scene: "withdraw-risk-check",
        request_time: "2026-10-30T10:00:00Z",
        inputs: &empty,
        facts: &facts,
    });
    assert_eq!(
        out[0].1,
        FactSample::Value { value: serde_json::json!(520), meta: None },
        "2*10+500 = 520"
    );
}

#[test]
fn derived_provider_separates_absent_from_broken_dependency() {
    let expr: gr_contract::Expr =
        serde_json::from_value(serde_json::json!({ "fact": "customer.withdrawCount24h" })).unwrap();

    let make = || {
        DerivedProvider::new(
            "echo",
            vec![("customer.withdrawCount24h".to_string(), FactKind::Integer)],
            BTreeMap::from([("customer.riskScore".to_string(), expr.clone())]),
        )
        .unwrap()
    };
    let p = make();
    let empty = BTreeMap::new();

    // Dependency not provided -> NOT_PROVIDED (upstream simply did not supply it)
    let out = p.fetch(&ProviderCtx {
        tenant_id: "pay-cn",
        scene: "s",
        request_time: "2026-10-30T10:00:00Z",
        inputs: &empty,
        facts: &empty,
    });
    assert_eq!(out[0].1, FactSample::missing(MissingReason::NotProvided));

    // Dependency provided but with the wrong type -> PROVIDER_ERROR (upstream supplied it wrongly; a different nature)
    let broken = inputs(&[("customer.withdrawCount24h", serde_json::json!("不是整数"))]);
    let out = p.fetch(&ProviderCtx {
        tenant_id: "pay-cn",
        scene: "s",
        request_time: "2026-10-30T10:00:00Z",
        inputs: &empty,
        facts: &broken,
    });
    assert_eq!(out[0].1, FactSample::missing(MissingReason::ProviderError));
}

#[test]
fn derived_provider_rejects_fact_not_declared_in_depends_on() {
    let expr: gr_contract::Expr = serde_json::from_value(serde_json::json!({
        "op": "gt",
        "args": [{ "fact": "customer.level" }, { "type": "integer", "value": 1 }]
    }))
    .unwrap();
    let err = DerivedProvider::new(
        "bad",
        vec![("customer.withdrawCount24h".to_string(), FactKind::Integer)],
        BTreeMap::from([("customer.riskScore".to_string(), expr)]),
    )
    .expect_err("未声明的依赖必须装载期拒绝");
    assert_eq!(err.error_code, ErrorCode::IrUnknownFact);
}

#[test]
fn derived_provider_rejects_bare_slot_reference() {
    let expr: gr_contract::Expr = serde_json::from_value(serde_json::json!({ "slot": 0 })).unwrap();
    let err = DerivedProvider::new("bad", vec![], BTreeMap::from([("x".to_string(), expr)]))
        .expect_err("不接受裸 slot");
    assert_eq!(err.error_code, ErrorCode::IrInvalid);
}

// ---------------------------------------------------------------- HTTP fetch

fn http_provider(mapping: &[(&str, &str)], t: Arc<dyn Transport>) -> HttpProvider {
    HttpProvider::new(
        "list-service",
        "http://127.0.0.1:9100/sanctions",
        mapping
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
        t,
        Duration::from_millis(50),
    )
}

fn view<'a>(empty: &'a BTreeMap<String, serde_json::Value>) -> ProviderCtx<'a> {
    ProviderCtx {
        tenant_id: "pay-cn",
        scene: "withdraw-risk-check",
        request_time: "2026-10-30T10:00:00Z",
        inputs: empty,
        facts: empty,
    }
}

#[test]
fn http_provider_maps_fields_by_path_and_carries_event_time() {
    let t = MockTransport::ok(
        r#"{"data":{"hit":true,"score":720},"meta":{"sanctions.hit":{"eventTime":"2026-10-30T09:59:58Z"}}}"#,
    );
    let p = http_provider(&[("sanctions.hit", "data.hit")], t.clone());
    let empty = BTreeMap::new();
    let out = p.fetch(&view(&empty));

    assert_eq!(t.calls.load(Ordering::Relaxed), 1);
    match &out[0].1 {
        FactSample::Value { value, meta } => {
            assert_eq!(value, &serde_json::json!(true));
            let meta = meta.as_ref().expect("应带 eventTime");
            assert_eq!(meta["eventTime"], serde_json::json!("2026-10-30T09:59:58Z"));
        }
        other => panic!("期望 Value，得到 {other:?}"),
    }
}

#[test]
fn http_provider_timeout_becomes_provider_timeout() {
    let p = http_provider(&[("sanctions.hit", "hit")], MockTransport::err(TransportError::Timeout));
    let empty = BTreeMap::new();
    assert_eq!(
        p.fetch(&view(&empty))[0].1,
        FactSample::missing(MissingReason::ProviderTimeout),
        "超时是 PROVIDER_TIMEOUT，不是 PROVIDER_ERROR"
    );
}

#[test]
fn http_provider_status_error_becomes_provider_error() {
    let p = http_provider(&[("sanctions.hit", "hit")], MockTransport::err(TransportError::Status(503)));
    let empty = BTreeMap::new();
    assert_eq!(
        p.fetch(&view(&empty))[0].1,
        FactSample::missing(MissingReason::ProviderError)
    );
}

#[test]
fn http_provider_absent_field_means_not_provided() {
    let p = http_provider(&[("sanctions.hit", "hit")], MockTransport::ok(r#"{"other":1}"#));
    let empty = BTreeMap::new();
    assert_eq!(
        p.fetch(&view(&empty))[0].1,
        FactSample::missing(MissingReason::NotProvided),
        "上游没这个字段 = 没提供，不是错误"
    );
}

#[test]
fn http_provider_marks_upstream_degraded_without_losing_value() {
    let t = MockTransport::ok(r#"{"hit":true,"degraded":true,"degradeReason":"STALE"}"#);
    let p = http_provider(&[("sanctions.hit", "hit")], t);
    let empty = BTreeMap::new();
    assert_eq!(
        p.fetch(&view(&empty))[0].1,
        FactSample::degraded(serde_json::json!(true), DegradeReason::Stale)
    );
}

#[test]
fn std_transport_refuses_https_and_reports_unreachable() {
    let err = StdTransport
        .get("https://example.com/facts", Duration::from_millis(20))
        .expect_err("P0 只支持 http://");
    assert!(matches!(err, TransportError::Unavailable(_)));

    // A local port nobody listens on: must return a classifiable error (not a panic, not a hang)
    let err = StdTransport
        .get("http://127.0.0.1:1/facts", Duration::from_millis(50))
        .expect_err("端口 1 不可用");
    assert!(matches!(
        err,
        TransportError::Unavailable(_) | TransportError::Timeout
    ));
}

// ---------------------------------------------------------------- Assemble assembly

#[test]
fn assembler_rejects_unknown_scene() {
    let a = Assembler::from_spec(&catalog(), &spec(r#"{"scenes":[]}"#)).unwrap();
    let err = a.assemble(&ctx("no-such-scene"), BTreeMap::new()).expect_err("场景未配置");
    assert_eq!(err.error_code, ErrorCode::AssembleSceneUnknown);
    assert_eq!(err.error_code.http_status(), 404);
}

#[test]
fn assembler_inputs_win_over_providers() {
    let a = Assembler::from_spec(
        &catalog(),
        &spec(
            r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[
                {"kind":"static","facts":{"config.withdrawDailyLimit":"100000"}}]}]}"#,
        ),
    )
    .unwrap();
    let snap = a
        .assemble(
            &ctx("withdraw-risk-check"),
            inputs(&[("config.withdrawDailyLimit", serde_json::json!("500"))]),
        )
        .unwrap();
    assert_eq!(
        snap.facts["config.withdrawDailyLimit"],
        serde_json::json!("500"),
        "调用方显式传入优先于 Provider"
    );
}

#[test]
fn assembler_registers_null_input_as_missing() {
    let a = Assembler::from_spec(
        &catalog(),
        &spec(r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[]}]}"#),
    )
    .unwrap();
    let snap = a
        .assemble(
            &ctx("withdraw-risk-check"),
            inputs(&[("sanctions.hit", serde_json::Value::Null)]),
        )
        .unwrap();
    assert!(!snap.facts.contains_key("sanctions.hit"), "禁止用 null 表达缺失");
    assert_eq!(snap.missing_facts.len(), 1);
    assert_eq!(snap.missing_facts[0].fact, "sanctions.hit");
    assert_eq!(snap.missing_facts[0].reason, MissingReason::NotProvided);
    assert!(snap.missing_facts[0].required, "必填性取事实目录，不由 Provider 决定");
}

#[test]
fn assembler_rejects_duplicate_fact_within_scene() {
    let err = Assembler::from_spec(
        &catalog(),
        &spec(
            r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[
                {"kind":"static","name":"a","facts":{"config.withdrawDailyLimit":"1"}},
                {"kind":"static","name":"b","facts":{"config.withdrawDailyLimit":"2"}}]}]}"#,
        ),
    )
    .expect_err("同场景重复供给必须装载期拒绝");
    assert_eq!(err.error_code, ErrorCode::IrInvalid);
    assert!(err.message.contains("config.withdrawDailyLimit"));
}

#[test]
fn assembler_rejects_fact_outside_catalog() {
    let err = Assembler::from_spec(
        &catalog(),
        &spec(
            r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[
                {"kind":"static","facts":{"no.such.fact":"1"}}]}]}"#,
        ),
    )
    .expect_err("目录外事实必须拒绝");
    assert_eq!(err.error_code, ErrorCode::IrUnknownFact);
}

#[test]
fn assembler_records_degraded_facts_with_values() {
    let t = MockTransport::ok(r#"{"hit":true,"degraded":true,"degradeReason":"PARTIAL"}"#);
    let a = Assembler::from_spec_with_transport(
        &catalog(),
        &spec(
            r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[
                {"kind":"http","url":"http://127.0.0.1:9100/sanctions","facts":{"sanctions.hit":"hit"}}]}]}"#,
        ),
        t,
    )
    .unwrap();
    let snap = a.assemble(&ctx("withdraw-risk-check"), BTreeMap::new()).unwrap();
    assert_eq!(snap.facts["sanctions.hit"], serde_json::json!(true));
    assert_eq!(snap.degraded_facts.len(), 1);
    assert_eq!(snap.degraded_facts[0].reason, DegradeReason::Partial);
    assert_eq!(snap.assembler_version.as_deref(), Some(gr_provider::ASSEMBLER_VERSION));
}

#[test]
fn snapshot_id_is_unique_and_carries_request_time() {
    let a = Assembler::from_spec(
        &catalog(),
        &spec(r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[]}]}"#),
    )
    .unwrap();
    let s1 = a.assemble(&ctx("withdraw-risk-check"), BTreeMap::new()).unwrap();
    let s2 = a.assemble(&ctx("withdraw-risk-check"), BTreeMap::new()).unwrap();
    assert_ne!(s1.snapshot_id, s2.snapshot_id, "同秒内也要唯一");
    assert!(s1.snapshot_id.starts_with("fs-20261030T100000Z"), "{}", s1.snapshot_id);
    assert_eq!(s1.fact_schema_version, "catalog-v2026.10");
    assert_eq!(s1.assembled_at.as_deref(), Some("2026-10-30T10:00:00Z"));
}

// ---------------------------------------------------------------- end-to-end

/// Main acceptance scenario: pass business inputs -> assemble the snapshot -> produce a decision, and it must **agree** with Snapshot mode.
#[test]
fn assemble_then_decide_matches_snapshot_mode() {
    let a = Assembler::from_spec(&catalog(), &spec(&read("providers.json"))).unwrap();
    let engine = engine_from_json(
        &read("fact-catalog.json"),
        &[read("rule-package.json")],
        EngineConfig::default(),
    )
    .unwrap();

    let mut c = ctx("withdraw-risk-check");
    c.ruleset_id = Some("rs-amount-limit".to_string());

    let business_inputs = inputs(&[
        ("request.amount", serde_json::json!("1500")),
        ("request.currency", serde_json::json!("CNY")),
        ("request.channel", serde_json::json!("APP")),
        ("sanctions.hit", serde_json::json!(false)),
        ("customer.withdrawCount24h", serde_json::json!(2)),
        ("customer.level", serde_json::json!("NORMAL")),
    ]);

    let snap = a.assemble(&c, business_inputs).unwrap();
    // Both the static and the derived Provider took effect
    assert_eq!(snap.facts["config.withdrawDailyLimit"], serde_json::json!("100000"));
    assert_eq!(snap.facts["customer.riskScore"], serde_json::json!(520), "2*10+500");
    // An unconfigured upstream (list service) does not block: the inputs already supply it, and only truly missing fields land in missingFacts
    assert!(snap.missing_facts.iter().all(|m| m.fact != "sanctions.hit"));

    let out = engine.decide(&snap, &c).expect("组装结果应可决策");
    assert_eq!(out.result.decision, gr_contract::Decision::Review);
    assert_eq!(out.result.reason_code, "AMOUNT_REVIEW");
    assert_eq!(out.result.snapshot_id, snap.snapshot_id);
    assert_eq!(out.result.outputs["riskLevel"], gr_contract::Value::Str("MEDIUM".into()));

    // Cross-check against Snapshot mode: the same facts through a full snapshot must reach the same conclusion
    let snapshot: gr_contract::FactSnapshot =
        serde_json::from_str(&read("snapshot.json")).unwrap();
    let direct = engine.decide(&snapshot, &c).expect("快照应可决策");
    assert_eq!(direct.result.decision, out.result.decision);
    assert_eq!(direct.result.reason_code, out.result.reason_code);
}

/// When nobody supplies a required fact, it is **not a 500**: B4's onMissing=ERROR lands a REJECT decision.
///
/// Note that this test does not assert the field is in `missingFacts` -- "nobody declared supplying it" and "declared but could not be fetched"
/// are two different things: the former is judged by the engine as `slot missing`, only the latter is registered by the assembler. Both paths lead to the same
/// fail-closed exit, and the test asserts the exit rather than the process.
#[test]
fn missing_required_fact_lands_reject_not_error() {
    let a = Assembler::from_spec(
        &catalog(),
        &spec(r#"{"scenes":[{"scene":"withdraw-risk-check","providers":[]}]}"#),
    )
    .unwrap();
    let engine = engine_from_json(
        &read("fact-catalog.json"),
        &[read("rule-package.json")],
        EngineConfig::default(),
    )
    .unwrap();

    let mut c = ctx("withdraw-risk-check");
    c.ruleset_id = Some("rs-amount-limit".to_string());

    let snap = a
        .assemble(&c, inputs(&[("request.amount", serde_json::json!("1500"))]))
        .unwrap();

    let out = engine.decide(&snap, &c).expect("契约违规应落决策而不是错误");
    assert_eq!(out.result.decision, gr_contract::Decision::Reject);
    assert_eq!(out.result.reason_code, "CONTRACT_VIOLATION");
    assert!(out.result.degraded);
    assert!(out
        .result
        .degraded_reasons
        .iter()
        .any(|d| d.fact == "config.withdrawDailyLimit"));
}
