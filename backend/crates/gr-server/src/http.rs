//! 极简 HTTP 类型（只够决策服务用：方法 + 路径 + 头 + body）。
//!
//! keep-alive（B12）需要的两件事就在这里：`Request::close` 与响应的 `Connection` 头。

use serde::Serialize;

pub struct Request {
    pub method: String,
    pub path: String,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    /// 客户端要求关闭连接（`Connection: close`，或 HTTP/1.0 且未显式 keep-alive）
    pub close: bool,
}

impl Request {
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(k, _)| k.eq_ignore_ascii_case(name))
            .map(|(_, v)| v.as_str())
    }
}

pub struct Response {
    pub status: u16,
    pub body: Vec<u8>,
}

impl Response {
    pub fn json<T: Serialize>(status: u16, body: &T) -> Self {
        let bytes = serde_json::to_vec(body).unwrap_or_else(|_| b"{}".to_vec());
        Self { status, body: bytes }
    }
}
