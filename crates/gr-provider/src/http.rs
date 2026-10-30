//! HTTP 拉取 Provider（名单服务、风险评分、聚合指标这类外部事实）。
//!
//! ## 传输层是可注入的
//!
//! `Transport` trait 把「怎么发请求」与「事实怎么映射」分开：
//! 单测注入 `MockTransport` 就能覆盖超时、5xx、坏 body 全部路径，不需要真起服务。
//!
//! ## P0 只支持 `http://`，且必须显式声明映射
//!
//! - 不支持 `https://`：手写 TLS 不可接受，引 TLS crate 又违背「决策服务依赖最小」。
//!   生产上应让 Provider 走内网 http 或本地 sidecar（该限制已记入文档开放项）。
//! - 映射必须显式：**不做「响应字段名撞事实名就自动认」的猜测**。事实名带命名空间
//!   （`customer.riskScore`），响应字段通常不带，靠猜迟早错配，而错配是资损级的。

use crate::sample::{FactSample, FactMeta, Provider, ProviderCtx};
use gr_contract::{DegradeReason, MissingReason};
use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::time::Duration;

#[derive(Clone, Debug, PartialEq)]
pub enum TransportError {
    Timeout,
    /// 连不上 / DNS 失败 / 连接被重置
    Unavailable(String),
    /// 非 2xx
    Status(u16),
    /// 响应不是合法 JSON
    Body(String),
}

/// 取数传输层（可注入，便于无网络单测）。
pub trait Transport: Send + Sync {
    fn get(&self, url: &str, timeout: Duration) -> Result<String, TransportError>;
}

/// 默认传输层：手写 HTTP/1.1 GET，仅 `http://`。
pub struct StdTransport;

impl Transport for StdTransport {
    fn get(&self, url: &str, timeout: Duration) -> Result<String, TransportError> {
        let rest = url.strip_prefix("http://").ok_or_else(|| {
            TransportError::Unavailable(format!("只支持 http:// （收到 {url}）"))
        })?;
        let (authority, path) = match rest.find('/') {
            Some(i) => (&rest[..i], &rest[i..]),
            None => (rest, "/"),
        };
        let host = authority.split(':').next().unwrap_or(authority);
        let port = authority
            .split_once(':')
            .and_then(|(_, p)| p.parse::<u16>().ok())
            .unwrap_or(80);

        let addr = (host, port)
            .to_socket_addrs()
            .map_err(|e| TransportError::Unavailable(format!("解析 {host}:{port} 失败: {e}")))?
            .next()
            .ok_or_else(|| TransportError::Unavailable(format!("{host}:{port} 无可用地址")))?;

        let mut stream = TcpStream::connect_timeout(&addr, timeout).map_err(|e| {
            if e.kind() == std::io::ErrorKind::TimedOut {
                TransportError::Timeout
            } else {
                TransportError::Unavailable(format!("连接 {addr} 失败: {e}"))
            }
        })?;
        stream.set_read_timeout(Some(timeout)).ok();
        stream.set_write_timeout(Some(timeout)).ok();

        let request = format!("GET {path} HTTP/1.1\r\nHost: {authority}\r\nConnection: close\r\n\r\n");
        stream.write_all(request.as_bytes()).map_err(|e| TransportError::Unavailable(e.to_string()))?;

        let mut raw = Vec::new();
        stream.read_to_end(&mut raw).map_err(|e| {
            if e.kind() == std::io::ErrorKind::WouldBlock || e.kind() == std::io::ErrorKind::TimedOut {
                TransportError::Timeout
            } else {
                TransportError::Unavailable(e.to_string())
            }
        })?;

        let text = String::from_utf8_lossy(&raw).to_string();
        let (head, body) = text
            .split_once("\r\n\r\n")
            .ok_or_else(|| TransportError::Body("响应缺少头部/主体分隔".to_string()))?;
        let status = head
            .lines()
            .next()
            .and_then(|line| line.split_whitespace().nth(1))
            .and_then(|s| s.parse::<u16>().ok())
            .unwrap_or(0);
        if !(200..300).contains(&status) {
            return Err(TransportError::Status(status));
        }
        Ok(body.to_string())
    }
}

pub struct HttpProvider {
    name: String,
    url: String,
    /// 事实名 → 响应体里的取值路径（`""` / `"$"` 表示「响应体本身是 事实名→值 的对象」）
    mapping: Vec<(String, String)>,
    provides: Vec<String>,
    transport: std::sync::Arc<dyn Transport>,
    timeout: Duration,
}

impl HttpProvider {
    pub fn new(
        name: impl Into<String>,
        url: impl Into<String>,
        mapping: BTreeMap<String, String>,
        transport: std::sync::Arc<dyn Transport>,
        timeout: Duration,
    ) -> Self {
        let provides: Vec<String> = mapping.keys().cloned().collect();
        Self {
            name: name.into(),
            url: url.into(),
            mapping: mapping.into_iter().collect(),
            provides,
            transport,
            timeout,
        }
    }

    /// 全部事实统一标记降级（例如上游返回 `degraded: true`）。
    fn degrade_all(
        &self,
        body: &serde_json::Value,
        reason: DegradeReason,
    ) -> Vec<(String, FactSample)> {
        self.mapping
            .iter()
            .filter_map(|(fact, path)| pick(body, path).map(|v| v.clone()).map(|v| (fact.clone(), FactSample::degraded(v, reason.clone()))))
            .collect()
    }
}

impl Provider for HttpProvider {
    fn name(&self) -> &str {
        &self.name
    }

    fn provides(&self) -> &[String] {
        &self.provides
    }

    fn fetch(&self, _ctx: &ProviderCtx<'_>) -> Vec<(String, FactSample)> {
        let raw = match self.transport.get(&self.url, self.timeout) {
            Ok(body) => body,
            // 超时与其它失败在契约里是**不同**的 reason 码（`contracts/02` §3.2），
            // 别合并成 PROVIDER_ERROR —— 排障时「慢」和「坏」要能分开。
            Err(TransportError::Timeout) => {
                return self
                    .mapping
                    .iter()
                    .map(|(f, _)| (f.clone(), FactSample::missing(MissingReason::ProviderTimeout)))
                    .collect()
            }
            Err(_) => {
                return self
                    .mapping
                    .iter()
                    .map(|(f, _)| (f.clone(), FactSample::missing(MissingReason::ProviderError)))
                    .collect()
            }
        };

        let body: serde_json::Value = match serde_json::from_str(&raw) {
            Ok(v) => v,
            Err(_) => {
                return self
                    .mapping
                    .iter()
                    .map(|(f, _)| (f.clone(), FactSample::missing(MissingReason::ProviderError)))
                    .collect()
            }
        };

        // 上游自报降级：值照用，但打标记（`contracts/02` §3.2 degraded 语义）
        if body.get("degraded").and_then(|v| v.as_bool()).unwrap_or(false) {
            let reason = body
                .get("degradeReason")
                .and_then(|v| v.as_str())
                .and_then(parse_degrade_reason)
                .unwrap_or(DegradeReason::ProviderDegraded);
            let degraded = self.degrade_all(&body, reason);
            if !degraded.is_empty() {
                return degraded;
            }
        }

        let mut out = Vec::with_capacity(self.mapping.len());
        for (fact, path) in &self.mapping {
            match pick(&body, path) {
                Some(v) if v.is_null() => {
                    out.push((fact.clone(), FactSample::missing(MissingReason::NotProvided)))
                }
                Some(v) => {
                    let meta = event_time_meta(&body, fact);
                    out.push((fact.clone(), FactSample::Value { value: v.clone(), meta }));
                }
                // 上游没这个字段 = 没提供，不是错误
                None => out.push((fact.clone(), FactSample::missing(MissingReason::NotProvided))),
            }
        }
        out
    }
}

/// 从响应里按路径取值：`""`/`"$"` 取整个对象，`a.b` 走点分路径。
fn pick<'a>(body: &'a serde_json::Value, path: &str) -> Option<&'a serde_json::Value> {
    if path.is_empty() || path == "$" {
        return Some(body);
    }
    let mut cur = body;
    for seg in path.split('.') {
        cur = cur.get(seg)?;
    }
    Some(cur)
}

/// `factMeta` 里给时效计算留的原料（`contracts/02` §5）。上游给了就带上。
fn event_time_meta(body: &serde_json::Value, fact: &str) -> Option<FactMeta> {
    let per_fact = body.get("meta")?.get(fact)?;
    if !per_fact.is_object() {
        return None;
    }
    let mut out = FactMeta::new();
    for key in ["eventTime", "sourceTimestamp"] {
        if let Some(v) = per_fact.get(key) {
            out.insert(key.to_string(), v.clone());
        }
    }
    if out.is_empty() {
        None
    } else {
        Some(out)
    }
}

fn parse_degrade_reason(s: &str) -> Option<DegradeReason> {
    match s {
        "STALE" => Some(DegradeReason::Stale),
        "PROVIDER_DEGRADED" => Some(DegradeReason::ProviderDegraded),
        "PARTIAL" => Some(DegradeReason::Partial),
        _ => None,
    }
}
