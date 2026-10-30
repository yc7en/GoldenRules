//! Minimal HTTP types (just enough for the decision service: method + path + headers + body).
//!
//! The two things keep-alive (B12) needs live right here: `Request::close` and the response `Connection` header.

use serde::Serialize;

pub struct Request {
    pub method: String,
    pub path: String,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    /// The client asked to close the connection (`Connection: close`, or HTTP/1.0 without an explicit keep-alive)
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
