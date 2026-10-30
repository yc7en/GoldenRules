//! B12 验收（服务层）：keep-alive、热重载、坏包不断服。
//!
//! 这些性质只能在**真实进程**上验证：进程内的 Engine 没有连接复用，也没有「重载失败」这种状态。

use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::PathBuf;
use std::process::{Child, Command};
use std::time::{Duration, Instant};

// ---------------------------------------------------------------- 脚手架

fn fixtures() -> PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../fixtures")
}

/// 起一个服务进程；`Drop` 时杀掉，避免测试失败留下野进程。
struct Server {
    child: Child,
    port: u16,
    dir: PathBuf,
}

impl Drop for Server {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn free_port() -> u16 {
    let l = TcpListener::bind("127.0.0.1:0").expect("bind");
    l.local_addr().unwrap().port()
}

/// 复制一份可改写的规则包到临时目录（重载测试需要改文件内容）
fn start(extra: &[&str]) -> Server {
    let port = free_port();
    let dir = std::env::temp_dir().join(format!("gr-server-test-{port}-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("mkdir");
    for name in ["fact-catalog.json", "rule-package.json"] {
        std::fs::copy(fixtures().join(name), dir.join(name)).expect("copy fixture");
    }

    let child = Command::new(env!("CARGO_BIN_EXE_gr-server"))
        .arg("--addr")
        .arg(format!("127.0.0.1:{port}"))
        .arg("--facts")
        .arg(dir.join("fact-catalog.json"))
        .arg("--package")
        .arg(dir.join("rule-package.json"))
        .args(extra)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .expect("spawn gr-server");

    let server = Server { child, port, dir };
    // 等端口就绪（最多 5s）
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if TcpStream::connect(("127.0.0.1", port)).is_ok() {
            return server;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    panic!("服务未在 5s 内就绪");
}

fn request(port: u16, raw: &str) -> String {
    let mut stream = TcpStream::connect(("127.0.0.1", port)).expect("connect");
    stream.set_read_timeout(Some(Duration::from_secs(5))).ok();
    stream.write_all(raw.as_bytes()).expect("write");
    read_one(&mut stream)
}

fn read_one(stream: &mut TcpStream) -> String {
    let mut buf = Vec::new();
    let mut chunk = [0u8; 4096];
    loop {
        let n = stream.read(&mut chunk).expect("read");
        if n == 0 {
            break;
        }
        buf.extend_from_slice(&chunk[..n]);
        let text = String::from_utf8_lossy(&buf).to_string();
        if let Some(pos) = text.find("\r\n\r\n") {
            let len: usize = text[..pos]
                .lines()
                .filter_map(|l| l.split_once(':'))
                .find(|(k, _)| k.eq_ignore_ascii_case("content-length"))
                .and_then(|(_, v)| v.trim().parse().ok())
                .unwrap_or(0);
            if buf.len() >= pos + 4 + len {
                return String::from_utf8_lossy(&buf[pos + 4..pos + 4 + len]).to_string();
            }
        }
    }
    String::new()
}

fn get(port: u16, path: &str) -> String {
    request(port, &format!("GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"))
}

const DECIDE_BODY: &str = r#"{"context":{"tenantId":"pay-cn","scene":"withdraw-risk-check","requestTime":"2026-10-30T10:00:00Z","rulesetId":"rs-amount-limit"},"snapshot":{"snapshotId":"fs-20261030-000123","tenantId":"pay-cn","scene":"withdraw-risk-check","factSchemaVersion":"catalog-v2026.10","facts":{"request.amount":"1500","request.currency":"CNY","request.channel":"APP","sanctions.hit":false,"customer.withdrawCount24h":2,"customer.level":"NORMAL","customer.riskScore":620,"config.withdrawDailyLimit":"100000"}}}"#;

fn decide_raw(keep_alive: bool) -> String {
    format!(
        "POST /v1/decide HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: {}\r\n\r\n{}",
        DECIDE_BODY.len(),
        if keep_alive { "keep-alive" } else { "close" },
        DECIDE_BODY
    )
}

// ---------------------------------------------------------------- keep-alive

#[test]
fn one_connection_serves_many_requests() {
    let s = start(&[]);
    let mut stream = TcpStream::connect(("127.0.0.1", s.port)).expect("connect");
    stream.set_read_timeout(Some(Duration::from_secs(5))).ok();

    let raw = decide_raw(true);
    for i in 0..5 {
        stream.write_all(raw.as_bytes()).expect("write");
        let body = read_one(&mut stream);
        assert!(
            body.contains("\"reasonCode\":\"AMOUNT_REVIEW\""),
            "第 {i} 次请求失败: {body}"
        );
    }
}

#[test]
fn connection_close_is_honoured() {
    let s = start(&[]);
    let mut stream = TcpStream::connect(("127.0.0.1", s.port)).expect("connect");
    stream.set_read_timeout(Some(Duration::from_secs(5))).ok();
    stream.write_all(decide_raw(false).as_bytes()).expect("write");
    let body = read_one(&mut stream);
    assert!(body.contains("AMOUNT_REVIEW"));
    // 服务端应主动关连接：再写会失败或读到 EOF
    let mut buf = [0u8; 1];
    let n = stream.read(&mut buf).unwrap_or(0);
    assert_eq!(n, 0, "Connection: close 之后应收到 EOF");
}

// ---------------------------------------------------------------- 热重载

#[test]
fn reload_bumps_generation_and_keeps_serving() {
    let s = start(&[]);
    let before: serde_json::Value = serde_json::from_str(&get(s.port, "/healthz")).unwrap();
    assert_eq!(before["generation"], 1);

    let reload = request(
        s.port,
        "POST /v1/admin/reload HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
    );
    let report: serde_json::Value = serde_json::from_str(&reload).expect("reload json");
    assert_eq!(report["generation"], 2);

    let after: serde_json::Value = serde_json::from_str(&get(s.port, "/healthz")).unwrap();
    assert_eq!(after["generation"], 2);
    // 重载后照样能出决策
    let body = request(s.port, &decide_raw(false));
    assert!(body.contains("AMOUNT_REVIEW"), "{body}");
}

#[test]
fn broken_package_does_not_replace_running_engine() {
    let s = start(&[]);
    // 把规则包写成坏 JSON，再请求重载
    std::fs::write(s.dir.join("rule-package.json"), b"{ this is not json }").expect("write");

    let resp = request(
        s.port,
        "POST /v1/admin/reload HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
    );
    assert!(
        resp.contains("IR_INVALID") || resp.contains("errorCode"),
        "坏包重载应返回错误体，实际: {resp}"
    );

    // 关键：旧引擎还在服务
    let body = request(s.port, &decide_raw(false));
    assert!(
        body.contains("AMOUNT_REVIEW"),
        "坏包重载后必须继续用旧引擎服务，实际: {body}"
    );
    let health: serde_json::Value = serde_json::from_str(&get(s.port, "/healthz")).unwrap();
    assert_eq!(health["generation"], 1, "失败的重载不应推进代次");
    assert_eq!(health["status"], "ok");
}

#[test]
fn reload_requires_token_when_configured() {
    let s = start(&["--reload-token", "s3cret"]);

    let denied = request(
        s.port,
        "POST /v1/admin/reload HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
    );
    assert!(denied.contains("X-Reload-Token"), "无 token 应被拒: {denied}");

    let ok = request(
        s.port,
        "POST /v1/admin/reload HTTP/1.1\r\nHost: localhost\r\nX-Reload-Token: s3cret\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
    );
    assert!(ok.contains("\"generation\":2"), "带正确 token 应成功: {ok}");
}

// ---------------------------------------------------------------- 资源上限自述

#[test]
fn limits_endpoint_reflects_cli_overrides() {
    let s = start(&["--max-inflight", "7", "--deadline-ms", "33"]);
    let limits: serde_json::Value = serde_json::from_str(&get(s.port, "/v1/limits")).unwrap();
    assert_eq!(limits["maxInflight"], 7);
    assert_eq!(limits["deadlineMs"], 33);
    assert_eq!(limits["inflight"], 0);

    let health: serde_json::Value = serde_json::from_str(&get(s.port, "/healthz")).unwrap();
    assert_eq!(health["maxInflight"], 7);
}

#[test]
fn overload_is_reported_as_503() {
    // max-inflight=0：任何决策请求都应被准入挡掉，且返回可识别的错误码
    let s = start(&["--max-inflight", "0"]);
    let body = request(s.port, &decide_raw(false));
    assert!(body.contains("EXEC_OVERLOAD"), "应返回过载错误，实际: {body}");
}

// ---------------------------------------------------------------- 发布门禁（F-PKG06）

/// 取 fixture 包，按需把首条用例改成 approved 并指定规则集。
fn candidate_package(approved: bool) -> serde_json::Value {
    let raw = std::fs::read_to_string(fixtures().join("rule-package.json")).expect("read package");
    let mut pkg: serde_json::Value = serde_json::from_str(&raw).expect("parse package");
    let tc = &mut pkg["tests"][0];
    // 该场景挂 3 个规则集，门禁要求用例写明 rulesetId
    tc["rulesetId"] = serde_json::json!("rs-amount-limit");
    tc["status"] = serde_json::json!(if approved { "approved" } else { "pending_review" });
    pkg
}

fn post_json(port: u16, path: &str, body: &serde_json::Value, token: Option<&str>) -> String {
    let payload = serde_json::to_string(body).unwrap();
    let auth = token
        .map(|t| format!("X-Reload-Token: {t}\r\n"))
        .unwrap_or_default();
    request(
        port,
        &format!(
            "POST {path} HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n{auth}Content-Length: {}\r\nConnection: close\r\n\r\n{payload}",
            payload.len()
        ),
    )
}

#[test]
fn verify_endpoint_runs_approved_cases() {
    let s = start(&[]);
    let body = serde_json::json!({ "package": candidate_package(true) });
    let out: serde_json::Value =
        serde_json::from_str(&post_json(s.port, "/v1/admin/verify", &body, None)).unwrap();
    assert_eq!(out["passed"], true, "应放行: {out}");
    assert_eq!(out["ran"], 1);
    assert_eq!(out["passedCount"], 1);
    assert_eq!(out["failedCount"], 0);
}

#[test]
fn verify_endpoint_blocks_failing_case() {
    let s = start(&[]);
    let mut pkg = candidate_package(true);
    // 把期望改成 REJECT：规则实际给 REVIEW，门禁必须拦下
    pkg["tests"][0]["expect"]["decision"] = serde_json::json!("REJECT");
    pkg["tests"][0]["expect"]["reasonCode"] = serde_json::json!("AMOUNT_LIMIT_EXCEEDED");
    let body = serde_json::json!({ "package": pkg });
    let out: serde_json::Value =
        serde_json::from_str(&post_json(s.port, "/v1/admin/verify", &body, None)).unwrap();
    assert_eq!(out["passed"], false, "应拦下: {out}");
    assert_eq!(out["failedCount"], 1);
    assert_eq!(out["failures"][0]["code"], "DECISION_MISMATCH");
    assert_eq!(out["failures"][0]["testId"], "T001");
}

#[test]
fn verify_endpoint_respects_token() {
    let s = start(&["--reload-token", "s3cret"]);
    let body = serde_json::json!({ "package": candidate_package(true) });
    let denied = post_json(s.port, "/v1/admin/verify", &body, None);
    assert!(denied.contains("X-Reload-Token"), "无 token 应被拒: {denied}");
    let ok = post_json(s.port, "/v1/admin/verify", &body, Some("s3cret"));
    assert!(ok.contains("\"passed\":true"), "带 token 应放行: {ok}");
}

#[test]
fn verify_endpoint_reports_ambiguous_scene() {
    let s = start(&[]);
    let mut pkg = candidate_package(true);
    pkg["tests"][0]
        .as_object_mut()
        .unwrap()
        .remove("rulesetId");
    let body = serde_json::json!({ "package": pkg });
    let out: serde_json::Value =
        serde_json::from_str(&post_json(s.port, "/v1/admin/verify", &body, None)).unwrap();
    assert_eq!(out["passed"], false);
    assert_eq!(out["failures"][0]["code"], "AMBIGUOUS_SCENE");
}
