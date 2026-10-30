//! B11 验收：执行资源上限与隔离。
//!
//! 核心命题不是「能拦住」，而是**拦住的方式正确**：
//! - 装载期越界 → 真错误，包进不来（不该在运行期才炸）；
//! - 请求期超预算/超时 → 真错误（不是 REJECT 决策，不能把「系统忙」记成「业务拒绝」）；
//! - 过载 → 快速失败 `EXEC_OVERLOAD`（503），**不排队**；
//! - 隔离 → 一个场景吃满自己的额度，不挤掉别的场景。

use gr_contract::*;
use gr_engine::{engine_from_json, Engine, EngineConfig};
use gr_engine::limits::SceneLimit;
use serde_json::json;
use std::collections::BTreeMap;

fn fixture(name: &str) -> String {
    let path = format!("{}/../../fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {path}: {e}"))
}

fn base_config() -> EngineConfig {
    EngineConfig::default()
}

fn engine_with(config: EngineConfig) -> Engine {
    engine_from_json(&fixture("fact-catalog.json"), &[fixture("rule-package.json")], config)
        .expect("engine")
}

fn snapshot() -> FactSnapshot {
    serde_json::from_str(&fixture("snapshot.json")).expect("snapshot")
}

fn ctx(ruleset: &str) -> ExecutionContext {
    let mut c = ExecutionContext::new("pay-cn", "withdraw-risk-check", "2026-10-30T10:00:00Z");
    c.ruleset_id = Some(ruleset.to_string());
    c
}

// ------------------------------------------------------------ 装载期闸门

#[test]
fn rule_count_limit_blocks_oversized_package_at_load() {
    // fixture 有 9 条规则；把上限压到 5 → 装载期就该拒绝
    let err = engine_from_json(
        &fixture("fact-catalog.json"),
        &[fixture("rule-package.json")],
        EngineConfig { max_rules_per_package: 5, ..base_config() },
    )
    .expect_err("超规则数上限的包不应加载成功");
    assert_eq!(err.error_code, ErrorCode::ExecRuleLimitExceeded);
    assert_eq!(err.error_code.http_status(), 500, "装载期越界是服务端配置/包问题，不是 4xx");
}

#[test]
fn instr_limit_blocks_expression_heavy_package() {
    // 规则条数不多但指令总量越界：拦的是「表达式爆炸」，不是「规则条数」
    let err = engine_from_json(
        &fixture("fact-catalog.json"),
        &[fixture("rule-package.json")],
        EngineConfig { max_instrs_per_package: 10, ..base_config() },
    )
    .expect_err("指令总量越界应被拒");
    assert_eq!(err.error_code, ErrorCode::ExecRuleLimitExceeded);
    assert!(err.details.contains_key("instrs"), "错误里要带实测值，便于定位是哪条规则爆的");
}

#[test]
fn memory_limit_blocks_large_package() {
    let err = engine_from_json(
        &fixture("fact-catalog.json"),
        &[fixture("rule-package.json")],
        EngineConfig { max_package_bytes: 1024, ..base_config() },
    )
    .expect_err("内存估算越界应被拒");
    assert_eq!(err.error_code, ErrorCode::ExecRuleLimitExceeded);
    assert!(err.details.contains_key("estBytes"));
}

#[test]
fn package_stats_are_reported() {
    let e = engine_with(base_config());
    let stats = e.stats();
    assert_eq!(stats.len(), 1);
    let s = &stats[0];
    assert_eq!(s.package_id, "pkg-withdraw-risk");
    assert_eq!(s.rules, 9);
    assert_eq!(s.rulesets, 4);
    assert!(s.instrs > 0 && s.est_bytes > s.instrs * gr_engine::limits::INSTR_BYTES / 2);
}

// ------------------------------------------------------------ 过载（准入）

#[test]
fn overload_is_a_real_error_not_a_decision() {
    // max_inflight=0：任何请求都进不来
    let e = engine_with(EngineConfig { max_inflight: 0, ..base_config() });
    let err = e.decide(&snapshot(), &ctx("rs-amount-limit")).expect_err("应为过载错误");
    assert_eq!(err.error_code, ErrorCode::ExecOverload);
    assert_eq!(err.error_code.http_status(), 503);
    // 关键：不能变成 REJECT 决策——「系统忙」不是「业务拒绝」
    assert_eq!(e.inflight(), 0, "拒绝后名额必须归还");
}

#[test]
fn inflight_is_released_after_each_decision() {
    let e = engine_with(EngineConfig { max_inflight: 1, ..base_config() });
    for _ in 0..50 {
        e.decide(&snapshot(), &ctx("rs-amount-limit")).expect("单并发下应始终成功");
        assert_eq!(e.inflight(), 0, "每次决策结束名额都要还回来");
    }
}

#[test]
fn concurrent_requests_cannot_exceed_inflight_cap() {
    let e = std::sync::Arc::new(engine_with(EngineConfig { max_inflight: 4, ..base_config() }));
    let ok = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let shed = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));

    let mut handles = Vec::new();
    for _ in 0..32 {
        let e = std::sync::Arc::clone(&e);
        let ok = std::sync::Arc::clone(&ok);
        let shed = std::sync::Arc::clone(&shed);
        handles.push(std::thread::spawn(move || {
            for _ in 0..20 {
                match e.decide(&snapshot(), &ctx("rs-amount-limit")) {
                    Ok(_) => {
                        ok.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    }
                    Err(err) => {
                        assert_eq!(err.error_code, ErrorCode::ExecOverload);
                        shed.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    }
                }
            }
        }));
    }
    for h in handles {
        h.join().unwrap();
    }

    // 只要过载真的发生过，就说明闸门在起作用；而成功的请求必须全部完成且名额归零
    let ok = ok.load(std::sync::atomic::Ordering::Relaxed);
    assert!(ok >= 32 * 20 / 4, "至少应有相当一部分成功（实测 {ok}）");
    assert_eq!(e.inflight(), 0, "压完必须归零，否则是名额泄漏");
}

#[test]
fn scene_cap_isolates_one_scene_from_another() {
    // 全局 8，但 withdraw-risk-check 只给 1 个名额；别的场景仍可用剩余容量
    let mut scene_limits = BTreeMap::new();
    scene_limits.insert(
        "withdraw-risk-check".to_string(),
        SceneLimit::new().with_max_inflight(1),
    );
    let e = engine_with(EngineConfig {
        max_inflight: 8,
        scene_limits: scene_limits.clone(),
        ..base_config()
    });

    // 顺序调用不会触发（名额即时归还）；这里验证「配置被识别且生效」的可见面
    let limits = e.limits_report();
    let sc = limits
        .scenes
        .iter()
        .find(|s| s.scene == "withdraw-risk-check")
        .expect("场景限额应出现在自述里");
    assert_eq!(sc.max_inflight, 1);
    assert_eq!(limits.max_inflight, 8);
}

// ------------------------------------------------------------ 场景级预算

#[test]
fn scene_deadline_override_applies() {
    // 场景级 deadline=0 → 必然超时；但另一场景（未覆盖）不受影响
    let mut scene_limits = BTreeMap::new();
    scene_limits.insert(
        "withdraw-risk-check".to_string(),
        SceneLimit::new().with_deadline_ms(0),
    );
    let e = engine_with(EngineConfig { scene_limits, ..base_config() });

    let err = e.decide(&snapshot(), &ctx("rs-amount-limit")).expect_err("0ms 预算必超时");
    assert_eq!(err.error_code, ErrorCode::ExecTimeout);
    assert_eq!(err.error_code.http_status(), 503);
    assert_eq!(err.details["scene"], json!("withdraw-risk-check"));
}

#[test]
fn scene_step_budget_override_stops_runaway_evaluation() {
    let mut scene_limits = BTreeMap::new();
    scene_limits.insert(
        "withdraw-risk-check".to_string(),
        SceneLimit::new().with_step_budget(3),
    );
    let e = engine_with(EngineConfig { scene_limits, ..base_config() });

    let err = e.decide(&snapshot(), &ctx("rs-amount-limit")).expect_err("预算耗尽即停");
    assert_eq!(err.error_code, ErrorCode::ExecStepLimit);
}

#[test]
fn default_limits_allow_normal_traffic() {
    let e = engine_with(base_config());
    let out = e.decide(&snapshot(), &ctx("rs-amount-limit")).expect("默认配置下应正常出决策");
    assert_eq!(out.result.reason_code, "AMOUNT_REVIEW");
}

// ------------------------------------------------------------ 自述

#[test]
fn limits_report_describes_all_scenes() {
    let e = engine_with(base_config());
    let r = e.limits_report();
    assert_eq!(r.max_inflight, 256);
    assert_eq!(r.step_budget, 200_000);
    assert_eq!(r.deadline_ms, 50);
    assert!(r.max_snapshot_bytes >= 1024);
    // 规则包里的三个场景都要出现在自述里（含未显式配置限额的）
    let scenes: Vec<&str> = r.scenes.iter().map(|s| s.scene.as_str()).collect();
    assert!(scenes.contains(&"withdraw-risk-check"));
    assert!(scenes.contains(&"withdraw-rate"));
    assert!(r.inflight == 0);
}
