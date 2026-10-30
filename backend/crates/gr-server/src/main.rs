//! 决策服务进程（HTTP/JSON 主入口，`ARCHITECTURE.md` §2）。
//!
//! 端点：
//! - `GET  /healthz`        存活 + 规则包可加载（PLAN B12 要探的就是这个）
//! - `GET  /v1/packages`    已加载规则包摘要 + 资源画像
//! - `GET  /v1/limits`      生效中的资源上限（B11 自述）
//! - `POST /v1/decide`      Snapshot 模式：传完整快照，纯求值
//! - `POST /v1/trial`       试跑：同 decide，但不落决策日志（平台 dry-run 用）
//! - `POST /v1/assemble`    Assemble 模式：传业务入参，服务内调 Provider 组装（B5）
//! - `POST /v1/admin/reload` 热重载规则包（B12：先编译后切换，发布零中断）
//! - `POST /v1/admin/verify` approved 用例发布门禁（F-PKG06：跑候选包，任一失败即阻断）
//!
//! 刻意用 `std::net` 手写极简 HTTP，而不是引 axum：
//! 决策服务对协议栈的要求只有「HTTP/1.1 + JSON + keep-alive」，多一个依赖就多一类升级风险。
//! keep-alive 是 B12 才补上的——没有它，压测量到的全是 TCP 建连成本，不是决策成本。

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

/// 空闲连接保持时间：太短会让 keep-alive 形同虚设，太长会堆积闲置线程
const IDLE_TIMEOUT: Duration = Duration::from_secs(30);
/// 单连接最多处理多少个请求（防止一个连接长期占用线程；到点主动关，客户端会重连）
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
            // Assemble 模式的 Provider 配置（不给则 /v1/assemble 返回 503）
            "--providers" => providers_path = args.next(),
            // 热重载鉴权：给了就必须带 X-Reload-Token，不给则只允许本机/内网使用
            "--reload-token" => reload_token = args.next(),
            // B11：资源上限（不给则用 EngineConfig::default）
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
                        // 客户端提前断开是常态（压测、连接池回收），不值得刷错误日志
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

/// 装载所需的全部输入（重载时按同样路径重读，所以路径要留住）。
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

/// 从磁盘装载引擎（可重入：启动与热重载共用同一条路径，避免两套行为）。
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

/// 服务实例：引擎可替换（热重载），因此放在 `RwLock` 里；请求路径只读。
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

    /// 热重载：**先编译后切换**——新包编译失败就保留旧引擎，服务不断。
    ///
    /// 这正是「发布零中断」的实现方式：编译期不持锁、不阻塞任何在途请求；
    /// 切换只是把 `Arc` 换掉，在途请求继续用完它手里的旧引擎，天然做到「预热到全部副本」。
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

/// 解析一个数值参数：写错了直接退出，不静默用默认值（资源上限被静默忽略是很危险的）。
fn parse_arg<T: std::str::FromStr>(args: &mut impl Iterator<Item = String>, flag: &str) -> T {
    let raw = args.next().unwrap_or_else(|| usage_exit(&format!("{flag} 缺少取值")));
    raw.parse::<T>()
        .unwrap_or_else(|_| usage_exit(&format!("{flag} 的取值不合法: {raw}")))
}

// ---------------------------------------------------------------- 连接循环

/// 一条连接处理多个请求（HTTP/1.1 keep-alive）。
///
/// 读用 `BufReader`（缓冲必须跨请求保留，否则每次重建会丢掉已缓冲的下一个请求），
/// 写用 `try_clone` 出来的独立句柄。
fn handle(stream: TcpStream, service: &Service) -> std::io::Result<()> {
    stream.set_read_timeout(Some(IDLE_TIMEOUT)).ok();
    stream.set_nodelay(true).ok();
    let mut writer = stream.try_clone()?;
    let mut reader = BufReader::new(stream);

    for _ in 0..MAX_REQUESTS_PER_CONN {
        let req = match read_request(&mut reader) {
            Ok(Some(r)) => r,
            Ok(None) => return Ok(()),
            // 空闲超时/客户端断开：正常收尾，不当错误
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
        // HTTP/1.0 默认关连接，HTTP/1.1 默认保持
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

// ---------------------------------------------------------------- 路由

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
        // B11：生效中的资源上限（压测与容量规划读它，不靠猜）
        ("GET", "/v1/limits") => Response::json(200, &service.engine().limits_report()),
        ("POST", "/v1/decide") => decide(req, service, true),
        ("POST", "/v1/trial") => decide(req, service, false),
        ("POST", "/v1/assemble") => assemble(req, service),
        ("POST", "/v1/admin/reload") => reload(req, service),
        // F-PKG06：发布门禁用例执行（拿候选包跑它自己的 approved 用例）
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
        // 探「规则包可加载」的第二个证据：当前代次（热重载后应递增）
        "generation": service.generation(),
        // Assemble 是否可用：探「Provider 配置装载成功」，探不到就是 503 而不是假健康
        "assembleScenes": service.assembler().map(|a| a.describe().len()).unwrap_or(0),
        // B11：在途数贴近上限时，扩容前先看这里
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

    // 取一次 Arc：热重载换引擎时，本次请求继续用旧引擎（在途一致性）
    let engine = service.engine();
    match engine.decide(&parsed.snapshot, &parsed.context) {
        Ok(outcome) => {
            if with_log {
                // P0：日志先打到 stdout（结构化 JSON），B10 接存储与查询
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
    /// 业务入参（事实名 → 原始值）。Provider 只补齐入参没给的那部分。
    #[serde(default)]
    inputs: std::collections::BTreeMap<String, serde_json::Value>,
}

/// Assemble 模式：传业务入参 → 内置 Provider 组装快照 → 同一套决策入口求值。
///
/// 响应只回 `DecisionResult`（决策契约 `contracts/03` 统一）：组装出的快照是中间产物，
/// 可能含 PII（`contracts/02` §9），不对调用方暴露。
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
        // 场景未配置 = 404；配置/目录不一致 = 500。都是「装配侧」的问题，不是决策。
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

/// 热重载（B12）：发布新的规则包文件后调用，**不中断在途请求**。
///
/// 失败语义：新包编译不过 → 返回 500 且**旧引擎继续服务**。
/// 「重载失败就把服务打挂」是最坏的选择：一次手滑的发布不该让整条链路停摆。
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
            // 注意：这里旧引擎仍在服务，HTTP 500 只表示「这次重载没成功」
            eprintln!("热重载失败（旧引擎保持服务）：{err}");
            Response::json(err.error_code.http_status(), &err)
        }
    }
}

#[allow(dead_code)]
fn outcome_summary(o: &DecisionOutcome) -> String {
    format!("{} {}", o.result.decision, o.result.reason_code)
}

// ---------------------------------------------------------------- 发布门禁

/// 管理员端点的统一鉴权。
///
/// 返回 `Some(response)` 表示应直接拒绝。思路是把「谁能改服务行为」收敛到一个判据里：
/// 热重载与用例门禁都是发布链路上的动作，不该出现「一个要 token、一个不用」的裂缝。
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

/// 发布门禁（F-PKG06）：拿候选包跑它自己的 approved 用例。
///
/// 入参只带包体，不带事实目录——门禁必须用**这台引擎真正在跑的那份目录**，
/// 否则「在我本地用另一份目录跑过了」就成了绕过门禁的口子。
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

    // 门禁是串行跑用例，不该被生产在途上限挡住；其余装载期硬顶照旧生效。
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
