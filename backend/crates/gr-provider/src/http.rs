//! HTTP fetch Provider (external facts such as list services, risk scores, aggregate metrics).
//!
//! ## The transport layer is injectable
//!
//! The `Transport` trait separates "how to send the request" from "how facts are mapped":
//! injecting a `MockTransport` in unit tests covers the timeout, 5xx and bad-body paths without starting a real server.
//!
//! ## P0 supports only `http://`, and the mapping must be declared explicitly
//!
//! - No `https://`: hand-rolling TLS is unacceptable, and pulling in a TLS crate violates "minimal dependencies for the decision service".
//!   In production the Provider should go through an intranet http endpoint or a local sidecar (this restriction is recorded as an open item in the docs).
//! - The mapping must be explicit: **no guessing along the lines of "if a response field name collides with a fact name, accept it"**. Fact names are namespaced
//!   (`customer.riskScore`) while response fields usually are not; guessing will mismatch eventually, and a mismatch costs money.

use crate::sample::{FactSample, FactMeta, Provider, ProviderCtx};
use gr_contract::{DegradeReason, MissingReason};
use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::time::Duration;

#[derive(Clone, Debug, PartialEq)]
pub enum TransportError {
    Timeout,
    /// Cannot connect / DNS failure / connection reset
    Unavailable(String),
    /// Non-2xx
    Status(u16),
    /// The response is not valid JSON
    Body(String),
}

/// Fetch transport layer (injectable, which makes network-free unit tests possible).
pub trait Transport: Send + Sync {
    fn get(&self, url: &str, timeout: Duration) -> Result<String, TransportError>;
}

/// Default transport: a hand-written HTTP/1.1 GET, `http://` only.
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
    /// Fact name -> value path in the response body (`""` / `"$"` means "the body itself is an object of fact name -> value")
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

    /// Mark every fact as degraded at once (e.g. the upstream returned `degraded: true`).
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
            // In the contract a timeout and any other failure are **different** reason codes (`contracts/02` §3.2);
            // never merge them into PROVIDER_ERROR -- when troubleshooting, "slow" and "broken" must stay distinguishable.
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

        // Upstream self-reported degradation: use the values anyway, but flag them (`contracts/02` §3.2 degraded semantics)
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
                // The upstream lacks this field = not provided, not an error
                None => out.push((fact.clone(), FactSample::missing(MissingReason::NotProvided))),
            }
        }
        out
    }
}

/// Pick a value from the response by path: `""`/`"$"` takes the whole object, `a.b` walks the dot-separated path.
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

/// The raw material kept in `factMeta` for freshness computation (`contracts/02` §5). Carry it along when the upstream provides it.
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
