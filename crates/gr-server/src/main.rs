//! 决策服务进程（HTTP/JSON 主入口，`ARCHITECTURE.md` §2）。
//!
//! 端点：
//! - `GET  /healthz`     存活 + 规则包可加载（PLAN B12 要探的就是这个）
//! - `GET  /v1/packages` 已加载规则包摘要
//! - `POST /v1/decide`   Snapshot 模式：传完整快照，纯求值
//! - `POST /v1/trial`    试跑：同 decide，但不落决策日志（平台 dry-run 用）
//! - `POST /v1/assemble` Assemble 模式：传业务入参，服务内调 Provider 组装（B5）
//!
//! 刻意用 `std::net` 手写极简 HTTP，而不是引 axum：
//! 决策服务对协议栈的要求只有「HTTP/1.1 + JSON + keep-alive 可关」，多一个依赖就多一类升级风险。

use gr_contract::{DecisionResult, ErrorContract, ExecutionContext, FactSnapshot};
use gr_engine::{engine_from_json, DecisionOutcome, Engine, EngineConfig};
use gr_provider::{Assembler, AssemblerSpec};
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::Arc;
use std::thread;

mod http;

use http::{Request, Response};

fn main() {
    let mut addr = "127.0.0.1:8080".to_string();
    let mut catalog_path: Option<String> = None;
    let mut package_paths: Vec<String> = Vec::new();
    let mut providers_path: Option<String> = None;

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
            "--help" | "-h" => {
                println!("用法: gr-server --addr 127.0.0.1:8080 --facts fixtures/fact-catalog.json --package fixtures/rule-package.json [--providers fixtures/providers.json]");
                return;
            }
            other => eprintln!("忽略未知参数: {other}"),
        }
    }

    let catalog_path = catalog_path.unwrap_or_else(|| usage_exit("缺少 --facts <事实目录 JSON>"));
    if package_paths.is_empty() {
        usage_exit("至少需要一个 --package <规则包 JSON>");
    }

    let catalog_json = read_file(&catalog_path);
    let package_jsons: Vec<String> = package_paths.iter().map(|p| read_file(p)).collect();

    let engine = match engine_from_json(&catalog_json, &package_jsons, EngineConfig::default()) {
        Ok(e) => Arc::new(e),
        Err(e) => {
            eprintln!("引擎启动失败: {e}");
            std::process::exit(1);
        }
    };

    for (id, ver, rulesets, rules) in engine.describe() {
        println!("已加载 {id} {ver}：{rulesets} 个规则集 / {rules} 条规则");
    }

    // Assemble 是可选的：不给配置就是纯 Snapshot 模式（不取数，A-2 的最保守形态）
    let assembler = providers_path.as_deref().map(|path| {
        let catalog: gr_contract::FactCatalog = serde_json::from_str(&catalog_json)
            .unwrap_or_else(|e| usage_exit(&format!("事实目录 JSON 解析失败: {e}")));
        let spec: AssemblerSpec = serde_json::from_str(&read_file(path))
            .unwrap_or_else(|e| usage_exit(&format!("Provider 配置解析失败: {e}")));
        match Assembler::from_spec(&catalog, &spec) {
            Ok(a) => {
                for (scene, names) in a.describe() {
                    println!("Assemble 场景 {scene}：{} 个 Provider（{}）", names.len(), names.join(", "));
                }
                Arc::new(a)
            }
            Err(e) => usage_exit(&format!("Provider 装配失败: {e}")),
        }
    });

    let service = Arc::new(Service { engine, assembler });

    let listener = match TcpListener::bind(&addr) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("监听 {addr} 失败: {e}");
            std::process::exit(1);
        }
    };
    println!("gr-server listening on http://{addr}");

    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let service = Arc::clone(&service);
                thread::spawn(move || {
                    if let Err(e) = handle(stream, &service) {
                        eprintln!("连接处理失败: {e}");
                    }
                });
            }
            Err(e) => eprintln!("accept 失败: {e}"),
        }
    }
}

/// 服务实例：Snapshot 模式只需要 engine；Assemble 模式额外需要 assembler。
struct Service {
    engine: Arc<Engine>,
    assembler: Option<Arc<Assembler>>,
}

fn usage_exit(msg: &str) -> ! {
    eprintln!("{msg}");
    eprintln!("用法: gr-server --addr 127.0.0.1:8080 --facts fixtures/fact-catalog.json --package fixtures/rule-package.json");
    std::process::exit(2);
}

fn read_file(path: &str) -> String {
    std::fs::read_to_string(path).unwrap_or_else(|e| {
        eprintln!("读取 {path} 失败: {e}");
        std::process::exit(1);
    })
}

fn handle(mut stream: TcpStream, service: &Service) -> std::io::Result<()> {
    let req = match read_request(&mut stream)? {
        Some(r) => r,
        None => return Ok(()),
    };

    let response = route(&req, service);
    write_response(&mut stream, response)
}

fn read_request(stream: &mut TcpStream) -> std::io::Result<Option<Request>> {
    let mut reader = BufReader::new(stream.try_clone()?);
    let mut request_line = String::new();
    if reader.read_line(&mut request_line)? == 0 {
        return Ok(None);
    }
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("").to_string();
    let path = parts.next().unwrap_or("/").to_string();

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
            if k.eq_ignore_ascii_case("content-length") {
                content_length = v.trim().parse().unwrap_or(0);
            }
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body)?;
    }

    Ok(Some(Request { method, path, body }))
}

fn write_response(stream: &mut TcpStream, resp: Response) -> std::io::Result<()> {
    let body = resp.body;
    let head = format!(
        "HTTP/1.1 {} {}\r\nContent-Type: application/json; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        resp.status,
        reason(resp.status),
        body.len()
    );
    stream.write_all(head.as_bytes())?;
    stream.write_all(&body)?;
    stream.flush()
}

fn reason(status: u16) -> &'static str {
    match status {
        200 => "OK",
        400 => "Bad Request",
        404 => "Not Found",
        409 => "Conflict",
        413 => "Payload Too Large",
        500 => "Internal Server Error",
        503 => "Service Unavailable",
        _ => "Unknown",
    }
}

fn route(req: &Request, service: &Service) -> Response {
    match (req.method.as_str(), req.path.as_str()) {
        ("GET", "/healthz") => Response::json(200, &health(service)),
        ("GET", "/v1/packages") => {
            let items: Vec<serde_json::Value> = service
                .engine
                .describe()
                .into_iter()
                .map(|(id, ver, rulesets, rules)| {
                    serde_json::json!({
                        "packageId": id, "packageVersion": ver,
                        "rulesets": rulesets, "rules": rules
                    })
                })
                .collect();
            Response::json(200, &serde_json::json!({ "packages": items }))
        }
        ("POST", "/v1/decide") => decide(req, &service.engine, true),
        ("POST", "/v1/trial") => decide(req, &service.engine, false),
        ("POST", "/v1/assemble") => assemble(req, service),
        ("GET", "/") => Response::json(
            200,
            &serde_json::json!({
                "service": "goldenrules-engine",
                "endpoints": ["/healthz", "/v1/packages", "/v1/decide", "/v1/trial", "/v1/assemble"],
                "assembleEnabled": service.assembler.is_some()
            }),
        ),
        _ => Response::json(
            404,
            &ErrorContract::new(gr_contract::ErrorCode::VersionRulesetNotFound, "未知路径"),
        ),
    }
}

fn health(service: &Service) -> serde_json::Value {
    let describe = service.engine.describe();
    serde_json::json!({
        "status": "ok",
        "engineVersion": gr_contract::ENGINE_VERSION,
        "packages": describe.len(),
        "rules": describe.iter().map(|(_, _, _, r)| *r).sum::<usize>(),
        "resultVersion": gr_contract::RESULT_VERSION,
        // Assemble 是否可用：探「Provider 配置装载成功」，探不到就是 503 而不是假健康
        "assembleScenes": service.assembler.as_ref().map(|a| a.describe().len()).unwrap_or(0),
    })
}

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct DecideRequest {
    context: ExecutionContext,
    snapshot: FactSnapshot,
}

fn decide(req: &Request, engine: &Engine, with_log: bool) -> Response {
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

    match engine.decide(&parsed.snapshot, &parsed.context) {
        Ok(outcome) => {
            if with_log {
                // P0：日志先打到 stdout（结构化 JSON），B10 接存储与查询
                if let Ok(line) = serde_json::to_string(&outcome.log) {
                    println!("{line}");
                }
                to_response(&outcome.result)
            } else {
                // 试跑：只返回决策，不落日志
                to_response(&outcome.result)
            }
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
    let Some(assembler) = service.assembler.as_ref() else {
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

    match service.engine.decide(&snapshot, &parsed.context) {
        Ok(outcome) => {
            if let Ok(line) = serde_json::to_string(&outcome.log) {
                println!("{line}");
            }
            to_response(&outcome.result)
        }
        Err(err) => Response::json(err.error_code.http_status(), &err),
    }
}

#[allow(dead_code)]
fn outcome_summary(o: &DecisionOutcome) -> String {
    format!("{} {}", o.result.decision, o.result.reason_code)
}
