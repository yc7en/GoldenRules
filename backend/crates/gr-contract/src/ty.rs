//! Type system and literals (`contracts/01` §3.3)
//!
//! `FactKind` is the type tag of a fact/literal; in IR a literal looks like
//! `{ "type": "decimal", "value": "1000" }` (`contracts/01` §7.3).

use crate::value::{serialize_tagged, Value};
use rust_decimal::Decimal;
use serde::de::{self, MapAccess, Visitor};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use std::fmt;
use std::str::FromStr;

pub(crate) fn is_false(v: &bool) -> bool {
    !*v
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum FactKind {
    Integer,
    Decimal,
    String,
    Boolean,
    Enum,
    Array,
}

impl FactKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            FactKind::Integer => "integer",
            FactKind::Decimal => "decimal",
            FactKind::String => "string",
            FactKind::Boolean => "boolean",
            FactKind::Enum => "enum",
            FactKind::Array => "array",
        }
    }

    pub fn parse(s: &str) -> Option<FactKind> {
        Some(match s {
            "integer" | "int" => FactKind::Integer,
            "decimal" | "number" => FactKind::Decimal,
            "string" => FactKind::String,
            "boolean" | "bool" => FactKind::Boolean,
            "enum" => FactKind::Enum,
            "array" => FactKind::Array,
            _ => return None,
        })
    }

    pub fn is_numeric(&self) -> bool {
        matches!(self, FactKind::Integer | FactKind::Decimal)
    }

    /// Convert a JSON value into an engine value by type (used by entry validation / snapshot loading).
    /// No cross-type implicit conversion: a type mismatch is an error, which B4 lands as `CONTRACT_TYPE_ERROR`.
    pub fn coerce(&self, json: &serde_json::Value) -> Result<Value, CoerceError> {
        use serde_json::Value as J;
        match self {
            FactKind::Integer => match json {
                J::Number(n) => n
                    .as_i64()
                    .map(Value::Int)
                    .ok_or_else(|| CoerceError::new("integer", n.to_string())),
                J::String(s) => s
                    .parse::<i64>()
                    .map(Value::Int)
                    .map_err(|_| CoerceError::new("integer", s.clone())),
                other => Err(CoerceError::new("integer", other.to_string())),
            },
            FactKind::Decimal => match json {
                J::String(s) => Decimal::from_str(s)
                    .map(Value::Dec)
                    .map_err(|_| CoerceError::new("decimal", s.clone())),
                J::Number(n) => Decimal::from_str(&n.to_string())
                    .map(Value::Dec)
                    .map_err(|_| CoerceError::new("decimal", n.to_string())),
                other => Err(CoerceError::new("decimal", other.to_string())),
            },
            FactKind::String | FactKind::Enum => match json {
                J::String(s) => Ok(Value::Str(s.clone())),
                other => Err(CoerceError::new(self.as_str(), other.to_string())),
            },
            FactKind::Boolean => match json {
                J::Bool(b) => Ok(Value::Bool(*b)),
                J::String(s) if s == "true" || s == "false" => Ok(Value::Bool(s == "true")),
                other => Err(CoerceError::new("boolean", other.to_string())),
            },
            FactKind::Array => match json {
                J::Array(items) => {
                    let mut out = Vec::with_capacity(items.len());
                    for item in items {
                        out.push(Value::from_json(item).map_err(|e| CoerceError::new("array", e))?);
                    }
                    Ok(Value::Arr(out))
                }
                other => Err(CoerceError::new("array", other.to_string())),
            },
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CoerceError {
    pub expected: String,
    pub actual: String,
}

impl CoerceError {
    pub fn new(expected: &str, actual: impl Into<String>) -> Self {
        Self { expected: expected.to_string(), actual: actual.into() }
    }
}

impl fmt::Display for CoerceError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "expect {}, got {}", self.expected, self.actual)
    }
}

impl std::error::Error for CoerceError {}

/// IR literal: `{ "type": <kind>, "value": <value> }`
#[derive(Clone, Debug, PartialEq)]
pub struct Literal {
    pub ty: FactKind,
    pub value: Value,
}

impl Literal {
    pub fn new(ty: FactKind, value: Value) -> Self {
        Self { ty, value }
    }

    pub fn int(v: i64) -> Self {
        Self { ty: FactKind::Integer, value: Value::Int(v) }
    }

    pub fn dec(v: &str) -> Self {
        Self { ty: FactKind::Decimal, value: Value::Dec(Decimal::from_str(v).expect("decimal")) }
    }

    pub fn string(v: &str) -> Self {
        Self { ty: FactKind::String, value: Value::Str(v.to_string()) }
    }

    pub fn boolean(v: bool) -> Self {
        Self { ty: FactKind::Boolean, value: Value::Bool(v) }
    }

    pub fn array(items: Vec<Value>) -> Self {
        Self { ty: FactKind::Array, value: Value::Arr(items) }
    }
}

impl Serialize for Literal {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        serialize_tagged(s, self.ty.as_str(), &self.value)
    }
}

impl<'de> Deserialize<'de> for Literal {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> Visitor<'de> for V {
            type Value = Literal;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                write!(f, "a literal object {{\"type\": .., \"value\": ..}}")
            }
            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Literal, A::Error> {
                let mut ty: Option<String> = None;
                let mut raw: Option<serde_json::Value> = None;
                while let Some(k) = map.next_key::<String>()? {
                    match k.as_str() {
                        "type" => ty = Some(map.next_value()?),
                        "value" => raw = Some(map.next_value()?),
                        other => return Err(de::Error::unknown_field(other, &["type", "value"])),
                    }
                }
                let ty = ty.ok_or_else(|| de::Error::missing_field("type"))?;
                let kind = FactKind::parse(&ty)
                    .ok_or_else(|| de::Error::custom(format!("unknown literal type: {ty}")))?;
                let raw = raw.ok_or_else(|| de::Error::missing_field("value"))?;
                let value = kind.coerce(&raw).map_err(de::Error::custom)?;
                Ok(Literal { ty: kind, value })
            }
        }
        d.deserialize_map(V)
    }
}
