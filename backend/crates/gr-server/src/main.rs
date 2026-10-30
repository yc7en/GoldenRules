//! Decision service process (main HTTP/JSON entry, `ARCHITECTURE.md` §2).
//!
//! Endpoints:
//! - `GET  /healthz`        liveness + rule package loadable (exactly what PLAN B12 wants to probe)
//! - `GET  /v1/packages`    summary of loaded rule packages + resource profile
//! - `GET  /v1/limits`      effective resource caps (B11 self-description)
//! - `POST /v1/decide`      Snapshot mode: pass a complete snapshot, pure evaluation
//! - `POST /v1/trial`       trial run: same as decide, but no decision log (for platform dry-run)
//! - `POST /v1/assemble`    Assemble mode: pass business inputs, the service calls Providers to assemble (B5)
//! - `POST /v1/admin/reload` hot reload of rule packages (B12: compile first, then swap; zero-interruption publish)
//! - `POST /v1/admin/verify` approved test-case publish gate (F-PKG06: run the candidate package, any failure blocks)
//!
//! Why hand-write a minimal HTTP stack on `std::net` instead of pulling in axum:
//! the decision service asks only "HTTP/1.1 + JSON + keep-alive" of a protocol stack, and every extra dependency is another upgrade risk.
//! keep-alive only arrived with B12 -- without it, bench numbers measured all TCP connect cost rather than decision cost.

use gr_contract::{DecisionResult, ErrorContract, ExecutionContext, FactCatalog, FactSnapshot};
use gr_engine::{engine_from_json, DecisionOutcome, Engine, EngineConfig};
use gr_provider::{Assembler, AssemblerSpec};
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::thread;
use std::time::Duration;

mod http;

use http::{Request, Response};

/// Idle connection keep-alive time: too short makes keep-alive meaningless, too long piles up idle threads
const IDLE_TIMEOUT: Duration = Duration::from_secs(30);
/// Max requests handled per connection (stops one connection from holding a thread indefinitely; we close on purpose at the limit and the client reconnects)
const MAX_REQUESTS_PER_CONN: usize = 100_000;

fn main() {
    let mut addr = "127.0.0.1:8080".to_string();
    let mut catalog_path: Option<String> = None;
    let mut package_paths: Vec<String> = Vec::new();
    let mut providers_path: Option<String> = None;
    let mut reload_token: Option<String> = None;
    let mut config = EngineConfig::default();

    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--addr" => addr = args.next().unwrap_or(addr),
            "--facts" => catalog_path = args.next(),
            "--package" | "-p" => {
                if let Some(p) = args.next() {
                    package_paths.push(p);
                }
            }
            // Provider config for Assemble mode (without it, /v1/assemble returns 503)
            "--providers" => providers_path = args.next(),
            // Hot-reload auth: when set, X-Reload-Token is mandatory; when unset, only localhost/intranet use is allowed
            "--reload-token" => reload_token = args.next(),
            // B11: resource caps (EngineConfig::default is used when not given)
            "--max-inflight" => config.max_inflight = parse_arg(&mut args, "--max-inflight"),
            "--deadline-ms" => config.deadline_ms = parse_arg(&mut args, "--deadline-ms"),
            "--step-budget" => config.step_budget = parse_arg(&mut args, "--step-budget"),
            "--max-rules" => config.max_rules_per_package = parse_arg(&mut args, "--max-rules"),
            "--max-instrs" => {
                config.max_instrs_per_package = parse_arg(&mut args, "--max-instrs");
            }
            "--max-package-bytes" => {
                config.max_package_bytes = parse_arg(&mut args, "--max-package-bytes");
            }
            "--max-snapshot-bytes" => {
                config.max_snapshot_bytes = parse_arg(&mut args, "--max-snapshot-bytes");
            }
            "--help" | "-h" => {
                println!("用法: gr-server --addr 127.0.0.1:8080 --facts fixtures/fact-catalog.json --package fixtures/rule-package.json [--providers fixtures/providers.json]");
                println!("资源上限: --max-inflight N --deadline-ms N --step-budget N --max-rules N --max-instrs N --max-package-bytes N --max-snapshot-bytes N");
                println!("热重载: --reload-token <token>（POST /v1/admin/reload 需带 X-Reload-Token）");
                return;
            }
            other => eprintln!("忽略未知参数: {other}"),
        }
    }

    let catalog_path = catalog_path.unwrap_or_else(|| usage_exit("缺少 --facts <事实目录 JSON>"));
    if package_paths.is_empty() {
        usage_exit("至少需要一个 --package <规则包 JSON>");
    }

    let sources = Sources {
        catalog_path,
        package_paths,
        providers_path,
        config: config.clone(),
    };

    let loaded = match load(&sources) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("引擎启动失败: {e}");
            std::process::exit(1);
        }
    };
    for (id, ver, rulesets, rules) in loaded.engine.describe() {
        println!("已加载 {id} {ver}：{rulesets} 个规则集 / {rules} 条规则");
    }
    for s in loaded.engine.stats() {
        println!(
            "资源画像 {} {}：{} 条指令 / 估算 {} KiB",
            s.package_id,
            s.package_version,
            s.instrs,
            s.est_bytes / 1024
        );
    }
    if sources.providers_path.is_some() {
        if let Some(a) = &loaded.assembler {
            for (scene, names) in a.describe() {
                println!("Assemble 场景 {scene}：{} 个 Provider（{}）", names.len(), names.join(", "));
            }
        }
    }
    if reload_token.is_none() {
        println!("警告：/v1/admin/reload 未设 --reload-token，仅应在内网/本机暴露该端口");
    }

    let service = Arc::new(Service {
        engine: RwLock::new(Arc::new(loaded.engine)),
        assembler: RwLock::new(loaded.assembler.map(Arc::new)),
        generation: AtomicU64::new(1),
        sources,
        reload_token,
    });

    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("监听 {addr} 失败: {e}");
            std::process::exit(1);
        }
    };
    println!("gr-server listening on http://{addr}（keep-alive 开启）");

    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let service = Arc::clone(&service);
                thread::spawn(move || {
                    if let Err(e) = handle(stream, &service) {
                        // A client disconnecting early is routine (load tests, pool recycling), not worth spamming the error log
                        if e.kind() != std::io::ErrorKind::ConnectionReset
                            && e.kind() != std::io::ErrorKind::BrokenPipe
                        {
                            eprintln!("连接处理失败: {e}");
                        }
                    }
                });
            }
            Err(e) => eprintln!("accept 失败: {e}"),
        }
    }
}

/// Every input needed to load (a reload re-reads the same paths, so the paths must be kept).
struct Sources {
    catalog_path: String,
    package_paths: Vec<String>,
    providers_path: Option<String>,
    config: EngineConfig,
}

struct Loaded {
    engine: Engine,
    assembler: Option<Assembler>,
}

/// Load the engine from disk (re-entrant: startup and hot reload share one path, so the two cannot behave differently).
fn load(sources: &Sources) -> Result<Loaded, ErrorContract> {
    let catalog_json = std::fs::read_to_string(&sources.catalog_path).map_err(|e| {
        ErrorContract::new(
            gr_contract::ErrorCode::IrInvalid,
            format!("读取事实目录 {} 失败: {e}", sources.catalog_path),
        )
    })?;
    let mut package_jsons = Vec::with_capacity(sources.package_paths.len());
    for path in &sources.package_paths {
        package_jsons.push(std::fs::read_to_string(path).map_err(|e| {
            ErrorContract::new(
                gr_contract::ErrorCode::IrInvalid,
                format!("读取规则包 {path} 失败: {e}"),
            )
        })?);
    }

    let engine = engine_from_json(&catalog_json, &package_jsons, sources.config.clone())?;

    let assembler = match &sources.providers_path {
        None => None,
        Some(path) => {
            let catalog: FactCatalog = serde_json::from_str(&catalog_json).map_err(|e| {
                ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("事实目录 JSON 解析失败: {e}"),
                )
            })?;
            let raw = std::fs::read_to_string(path).map_err(|e| {
                ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("读取 Provider 配置 {path} 失败: {e}"),
                )
            })?;
            let spec: AssemblerSpec = serde_json::from_str(&raw).map_err(|e| {
                ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("Provider 配置解析失败: {e}"),
                )
            })?;
            Some(Assembler::from_spec(&catalog, &spec)?)
        }
    };

    Ok(Loaded { engine, assembler })
}

/// Service instance: the engine can be swapped (hot reload), so it lives in an `RwLock`; the request path is read-only.
struct Service {
    engine: RwLock<Arc<Engine>>,
    assembler: RwLock<Option<Arc<Assembler>>>,
    generation: AtomicU64,
    sources: Sources,
    reload_token: Option<String>,
}

impl Service {
    fn engine(&self) -> Arc<Engine> {
        self.engine.read().unwrap_or_else(|e| e.into_inner()).clone()
    }

    fn assembler(&self) -> Option<Arc<Assembler>> {
        self.assembler.read().unwrap_or_else(|e| e.into_inner()).clone()
    }

    fn generation(&self) -> u64 {
        self.generation.load(Ordering::Acquire)
    }

    /// Hot reload: **compile first, then swap** -- if the new package fails to compile, the old engine stays and the service keeps running.
    ///
    /// This is exactly how "zero-interruption publish" is implemented: no lock is held and no in-flight request is blocked during compilation;
    /// the swap just replaces the `Arc`, and in-flight requests keep using the old engine they already hold, which naturally achieves "warm up every replica".
    fn reload(&self) -> Result<serde_json::Value, ErrorContract> {
        let loaded = load(&self.sources)?;
        let describe = loaded.engine.describe();
        let stats = loaded.engine.stats();

        let generation = self.generation.fetch_add(1, Ordering::AcqRel) + 1;
        *self.engine.write().unwrap_or_else(|e| e.into_inner()) = Arc::new(loaded.engine);
        *self.assembler.write().unwrap_or_else(|e| e.into_inner()) =
            loaded.assembler.map(Arc::new);

        Ok(serde_json::json!({
            "generation": generation,
            "packages": describe.iter().map(|(id, ver, rulesets, rules)| serde_json::json!({
                "packageId": id, "packageVersion": ver, "rulesets": rulesets, "rules": rules
            })).collect::<Vec<_>>(),
            "instrs": stats.iter().map(|s| s.instrs).sum::<usize>(),
        }))
    }
}

fn usage_exit(msg: &str) -> ! {
    eprintln!("{msg}");
    eprintln!("用法: gr-server --addr 127.0.0.1:8080 --facts fixtures/fact-catalog.json --package fixtures/rule-package.json");
    std::process::exit(2);
}

/// Parse a numeric argument: a typo exits immediately rather than silently using the default (a silently ignored resource cap is very dangerous).
fn parse_arg<T: std::str::FromStr>(args: &mut impl Iterator<Item = String>, flag: &str) -> T {
    let raw = args.next().unwrap_or_else(|| usage_exit(&format!("{flag} 缺少取值")));
    raw.parse::<T>()
        .unwrap_or_else(|_| usage_exit(&format!("{flag} 的取值不合法: {raw}")))
}

// ---------------------------------------------------------------- connection loop

/// Handle multiple requests on one connection (HTTP/1.1 keep-alive).
///
/// Reads go through a `BufReader` (the buffer must survive across requests, otherwise rebuilding it would drop the next request already buffered),
/// writes go through a separate handle obtained from `try_clone`.
fn handle(stream: TcpStream, service: &Service) -> std::io::Result<()> {
    stream.set_read_timeout(Some(IDLE_TIMEOUT)).ok();
    stream.set_nodelay(true).ok();
    let mut writer = stream.try_clone()?;
    let mut reader = BufReader::new(stream);

    for _ in 0..MAX_REQUESTS_PER_CONN {
        let req = match read_request(&mut reader) {
            Ok(Some(r)) => r,
            Ok(None) => return Ok(()),
            // Idle timeout / client disconnect: normalize the shutdown, do not treat it as an error
            Err(e)
                if e.kind() == std::io::ErrorKind::WouldBlock
                    || e.kind() == std::io::ErrorKind::TimedOut =>
            {
                return Ok(())
            }
            Err(e) => return Err(e),
        };

        let close = req.close;
        let response = route(&req, service);
        write_response(&mut writer, response, !close)?;
        if close {
            return Ok(());
        }
    }
    Ok(())
}

fn read_request(reader: &mut BufReader<TcpStream>) -> std::io::Result<Option<Request>> {
    let mut request_line = String::new();
    if reader.read_line(&mut request_line)? == 0 {
        return Ok(None);
    }
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("").to_string();
    let path = parts.next().unwrap_or("/").to_string();
    let version = parts.next().unwrap_or("HTTP/1.1").to_string();

    let mut headers: Vec<(String, String)> = Vec::new();
    let mut content_length = 0usize;
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line)? == 0 {
            break;
        }
        let trimmed = line.trim_end();
        if trimmed.is_empty() {
            break;
        }
        if let Some((k, v)) = trimmed.split_once(':') {
            let (k, v) = (k.trim().to_string(), v.trim().to_string());
            if k.eq_ignore_ascii_case("content-length") {
                content_length = v.parse().unwrap_or(0);
            }
            headers.push((k, v));
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body)?;
    }

    let conn = headers
        .iter()
        .find(|(k, _)| k.eq_ignore_ascii_case("connection"))
        .map(|(_, v)| v.to_ascii_lowercase());
    let close = match conn.as_deref() {
        Some(v) if v.contains("close") => true,
        Some(v) if v.contains("keep-alive") => false,
        // HTTP/1.0 closes by default, HTTP/1.1 keeps alive by default
        _ => version.eq_ignore_ascii_case("HTTP/1.0"),
    };

    Ok(Some(Request { method, path, headers, body, close }))
}

fn write_response(stream: &mut TcpStream, resp: Response, keep_alive: bool) -> std::io::Result<()> {
    let body = resp.body;
    let head = format!(
        "HTTP/1.1 {} {}\r\nContent-Type: application/json; charset=utf-8\r\nContent-Length: {}\r\nConnection: {}\r\n\r\n",
        resp.status,
        reason(resp.status),
        body.len(),
        if keep_alive { "keep-alive" } else { "close" }
    );
    stream.write_all(head.as_bytes())?;
    stream.write_all(&body)?;
    stream.flush()
}

fn reason(status: u16) -> &'static str {
    match status {
        200 => "OK",
        400 => "Bad Request",
        401 => "Unauthorized",
        404 => "Not Found",
        409 => "Conflict",
        413 => "Payload Too Large",
        500 => "Internal Server Error",
        503 => "Service Unavailable",
        _ => "Unknown",
    }
}

// ---------------------------------------------------------------- routing

fn route(req: &Request, service: &Service) -> Response {
    match (req.method.as_str(), req.path.as_str()) {
        ("GET", "/healthz") => Response::json(200, &health(service)),
        ("GET", "/v1/packages") => {
            let engine = service.engine();
            let stats = engine.stats();
            let items: Vec<serde_json::Value> = engine
                .describe()
                .into_iter()
                .map(|(id, ver, rulesets, rules)| {
                    let st = stats
                        .iter()
                        .find(|s| s.package_id == id && s.package_version == ver);
                    serde_json::json!({
                        "packageId": id, "packageVersion": ver,
                        "rulesets": rulesets, "rules": rules,
                        "instrs": st.map(|s| s.instrs),
                        "estBytes": st.map(|s| s.est_bytes),
                    })
                })
                .collect();
            Response::json(200, &serde_json::json!({ "packages": items }))
        }
        // B11: effective resource caps (benchmarking and capacity planning read this instead of guessing)
        ("GET", "/v1/limits") => Response::json(200, &service.engine().limits_report()),
        ("POST", "/v1/decide") => decide(req, service, true),
        ("POST", "/v1/trial") => decide(req, service, false),
        ("POST", "/v1/assemble") => assemble(req, service),
        ("POST", "/v1/admin/reload") => reload(req, service),
        // F-PKG06: publish-gate test-case execution (run the candidate package's own approved cases)
        ("POST", "/v1/admin/verify") => verify(req, service),
        ("GET", "/") => Response::json(
            200,
            &serde_json::json!({
                "service": "goldenrules-engine",
                "endpoints": ["/healthz", "/v1/packages", "/v1/limits", "/v1/decide",
                              "/v1/trial", "/v1/assemble", "/v1/admin/reload",
                              "/v1/admin/verify"],
                "assembleEnabled": service.assembler().is_some(),
                "generation": service.generation(),
            }),
        ),
        _ => Response::json(
            404,
            &ErrorContract::new(gr_contract::ErrorCode::VersionRulesetNotFound, "未知路径"),
        ),
    }
}

fn health(service: &Service) -> serde_json::Value {
    let engine = service.engine();
    let describe = engine.describe();
    let limits = engine.limits_report();
    serde_json::json!({
        "status": "ok",
        "engineVersion": gr_contract::ENGINE_VERSION,
        "packages": describe.len(),
        "rules": describe.iter().map(|(_, _, _, r)| *r).sum::<usize>(),
        "resultVersion": gr_contract::RESULT_VERSION,
        // Second piece of evidence for "the rule package is loadable": the current generation (it should increment after a hot reload)
        "generation": service.generation(),
        // Whether Assemble is available: probes "the Provider config loaded"; if it cannot be probed, that is a 503 rather than fake health
        "assembleScenes": service.assembler().map(|a| a.describe().len()).unwrap_or(0),
        // B11: when in-flight approaches the cap, look here before scaling out
        "inflight": limits.inflight,
        "maxInflight": limits.max_inflight,
    })
}

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct DecideRequest {
    context: ExecutionContext,
    snapshot: FactSnapshot,
}

fn decide(req: &Request, service: &Service, with_log: bool) -> Response {
    let parsed: DecideRequest = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            return Response::json(
                400,
                &ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("请求体不是合法的决策请求: {e}"),
                ),
            )
        }
    };

    // Grab the Arc once: when a hot reload swaps the engine, this request keeps using the old one (in-flight consistency)
    let engine = service.engine();
    match engine.decide(&parsed.snapshot, &parsed.context) {
        Ok(outcome) => {
            if with_log {
                // P0: logs go to stdout first (structured JSON); B10 wires up storage and query
                if let Ok(line) = serde_json::to_string(&outcome.log) {
                    println!("{line}");
                }
            }
            to_response(&outcome.result)
        }
        Err(err) => Response::json(err.error_code.http_status(), &err),
    }
}

fn to_response(result: &DecisionResult) -> Response {
    Response::json(200, result)
}

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct AssembleRequest {
    context: ExecutionContext,
    /// Business inputs (fact name -> raw value). Providers only fill in what the inputs did not supply.
    #[serde(default)]
    inputs: std::collections::BTreeMap<String, serde_json::Value>,
}

/// Assemble mode: pass business inputs -> built-in Providers assemble the snapshot -> the same decision entry evaluates it.
///
/// The response only returns `DecisionResult` (unified by the decision contract `contracts/03`): the assembled snapshot is an intermediate product
/// that may contain PII (`contracts/02` §9), so it is not exposed to callers.
fn assemble(req: &Request, service: &Service) -> Response {
    let Some(assembler) = service.assembler() else {
        return Response::json(
            503,
            &ErrorContract::new(
                gr_contract::ErrorCode::ProviderUnavailable,
                "未配置 Provider（启动时缺少 --providers）",
            ),
        );
    };

    let parsed: AssembleRequest = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            return Response::json(
                400,
                &ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("请求体不是合法的组装请求: {e}"),
                ),
            )
        }
    };

    let snapshot = match assembler.assemble(&parsed.context, parsed.inputs) {
        Ok(s) => s,
        // Scene not configured = 404; config/catalog mismatch = 500. Both are "assembly-side" problems, not decisions.
        Err(err) => return Response::json(err.error_code.http_status(), &err),
    };

    let engine = service.engine();
    match engine.decide(&snapshot, &parsed.context) {
        Ok(outcome) => {
            if let Ok(line) = serde_json::to_string(&outcome.log) {
                println!("{line}");
            }
            to_response(&outcome.result)
        }
        Err(err) => Response::json(err.error_code.http_status(), &err),
    }
}

/// Hot reload (B12): called after publishing new rule package files, **without interrupting in-flight requests**.
///
/// Failure semantics: if the new package fails to compile -> return 500 and **the old engine keeps serving**.
/// "Take the service down when a reload fails" is the worst option: one slipshod publish must not stall the whole chain.
fn reload(req: &Request, service: &Service) -> Response {
    if let Some(denied) = admin_denied(req, service) {
        return denied;
    }

    match service.reload() {
        Ok(report) => {
            println!(
                "热重载完成：generation={} instrs={}",
                report["generation"],
                report["instrs"]
            );
            Response::json(200, &report)
        }
        Err(err) => {
            // Note: the old engine is still serving here; the HTTP 500 only means "this reload did not succeed"
            eprintln!("热重载失败（旧引擎保持服务）：{err}");
            Response::json(err.error_code.http_status(), &err)
        }
    }
}

#[allow(dead_code)]
fn outcome_summary(o: &DecisionOutcome) -> String {
    format!("{} {}", o.result.decision, o.result.reason_code)
}

// ---------------------------------------------------------------- publish gate

/// Unified auth for admin endpoints.
///
/// Returning `Some(response)` means the request should be rejected outright. The idea is to converge "who may change service behavior" into a single criterion:
/// hot reload and the test-case gate are both actions on the publish path, and there must be no crack where "one needs a token and the other does not".
fn admin_denied(req: &Request, service: &Service) -> Option<Response> {
    let expected = service.reload_token.as_ref()?;
    match req.header("x-reload-token") {
        Some(got) if got == expected => None,
        _ => Some(Response::json(
            401,
            &ErrorContract::new(
                gr_contract::ErrorCode::IrInvalid,
                "该管理端点需要正确的 X-Reload-Token",
            ),
        )),
    }
}

/// Publish gate (F-PKG06): run the candidate package's own approved test cases.
///
/// The request carries only the package, not the fact catalog -- the gate must use **the very catalog this engine is running**,
/// otherwise "I already ran it locally against a different catalog" becomes a hole for bypassing the gate.
#[derive(serde::Deserialize)]
struct VerifyRequest {
    package: gr_contract::RulePackage,
}

fn verify(req: &Request, service: &Service) -> Response {
    if let Some(denied) = admin_denied(req, service) {
        return denied;
    }
    let body: VerifyRequest = match serde_json::from_slice(&req.body) {
        Ok(v) => v,
        Err(e) => {
            return Response::json(
                400,
                &ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("请求体解析失败（需要 {{\"package\": {{...}}}}）: {e}"),
                ),
            )
        }
    };

    let catalog_json = match std::fs::read_to_string(&service.sources.catalog_path) {
        Ok(v) => v,
        Err(e) => {
            return Response::json(
                500,
                &ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("读取事实目录 {} 失败: {e}", service.sources.catalog_path),
                ),
            )
        }
    };
    let catalog: FactCatalog = match serde_json::from_str(&catalog_json) {
        Ok(v) => v,
        Err(e) => {
            return Response::json(
                500,
                &ErrorContract::new(
                    gr_contract::ErrorCode::IrInvalid,
                    format!("事实目录 JSON 解析失败: {e}"),
                ),
            )
        }
    };

    // The gate runs test cases serially and must not be blocked by the production in-flight cap; every other load-time hard cap still applies.
    let mut config = service.sources.config.clone();
    config.max_inflight = usize::MAX;

    match gr_engine::verify::verify_package(&body.package, &catalog, config) {
        Ok(report) => {
            let failures: Vec<serde_json::Value> = report
                .failures
                .iter()
                .map(|f| {
                    serde_json::json!({
                        "testId": f.test_id, "code": f.code, "message": f.message
                    })
                })
                .collect();
            let passed = report.ok();
            if !passed {
                eprintln!(
                    "用例门禁未通过：包 {} 有 {} 条用例失败",
                    body.package.package_id, report.failed
                );
            }
            Response::json(
                200,
                &serde_json::json!({
                    "passed": passed,
                    "summary": report.summary(),
                    "total": report.total,
                    "ran": report.ran,
                    "passedCount": report.passed,
                    "failedCount": report.failed,
                    "skipped": report.skipped,
                    "failures": failures,
                    "packageId": body.package.package_id,
                    "packageVersion": body.package.package_version,
                }),
            )
        }
        Err(err) => Response::json(err.error_code.http_status(), &err),
    }
}
