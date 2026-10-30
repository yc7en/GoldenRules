//! 极简 HTTP 类型（只够决策服务用：方法 + 路径 + body）。

use serde::Serialize;

pub struct Request {
    pub method: String,
    pub path: String,
    pub body: Vec<u8>,
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
