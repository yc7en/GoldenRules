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
            "--help" | "-h" => {
                println!("用法: gr-server --addr 127.0.0.1:8080 --facts fixtures/fact-catalog.json --package fixtures/rule-package.json");
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
                let engine = Arc::clone(&engine);
                thread::spawn(move || {
                    if let Err(e) = handle(stream, &engine) {
                        eprintln!("连接处理失败: {e}");
                    }
                });
            }
            Err(e) => eprintln!("accept 失败: {e}"),
        }
    }
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

fn handle(mut stream: TcpStream, engine: &Engine) -> std::io::Result<()> {
    let req = match read_request(&mut stream)? {
        Some(r) => r,
        None => return Ok(()),
    };

    let response = route(&req, engine);
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

fn route(req: &Request, engine: &Engine) -> Response {
    match (req.method.as_str(), req.path.as_str()) {
        ("GET", "/healthz") => Response::json(200, &health(engine)),
        ("GET", "/v1/packages") => {
            let items: Vec<serde_json::Value> = engine
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
        ("POST", "/v1/decide") => decide(req, engine, true),
        ("POST", "/v1/trial") => decide(req, engine, false),
        ("POST", "/v1/assemble") => Response::json(
            503,
            &ErrorContract::new(
                gr_contract::ErrorCode::ProviderUnavailable,
                "Assemble 入口在 B5 提供",
            ),
        ),
        ("GET", "/") => Response::json(
            200,
            &serde_json::json!({
                "service": "goldenrules-engine",
                "endpoints": ["/healthz", "/v1/packages", "/v1/decide", "/v1/trial", "/v1/assemble"]
            }),
        ),
        _ => Response::json(
            404,
            &ErrorContract::new(gr_contract::ErrorCode::VersionRulesetNotFound, "未知路径"),
        ),
    }
}

fn health(engine: &Engine) -> serde_json::Value {
    serde_json::json!({
        "status": "ok",
        "engineVersion": gr_contract::ENGINE_VERSION,
        "packages": engine.describe().len(),
        "rules": engine.describe().iter().map(|(_, _, _, r)| *r).sum::<usize>(),
        "resultVersion": gr_contract::RESULT_VERSION,
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

#[allow(dead_code)]
fn outcome_summary(o: &DecisionOutcome) -> String {
    format!("{} {}", o.result.decision, o.result.reason_code)
}
