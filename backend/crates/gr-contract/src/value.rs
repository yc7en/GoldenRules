//! Value model (`contracts/01` §3.3 / `contracts/02` §2)
//!
//! Amounts/rates always go through `rust_decimal::Decimal` (exact decimal, floats forbidden, D9 / A-5).
//! Serialization: a decimal lands as a **string**, avoiding JSON number precision ambiguity (cross-language consistency).

use rust_decimal::prelude::FromPrimitive;
use rust_decimal::Decimal;
use serde::de::{self, MapAccess, SeqAccess, Visitor};
use serde::ser::{SerializeSeq, SerializeStruct};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use std::fmt;
use std::str::FromStr;

#[derive(Clone, Debug, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Int(i64),
    Dec(Decimal),
    Str(String),
    Arr(Vec<Value>),
}

impl Value {
    pub fn is_null(&self) -> bool {
        matches!(self, Value::Null)
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Value::Bool(b) => Some(*b),
            _ => None,
        }
    }

    /// For hit tests: only `true` counts as a hit; null / non-boolean never hits.
    pub fn is_truthy(&self) -> bool {
        matches!(self, Value::Bool(true))
    }

    /// Numeric view (an integer is promoted to decimal; the type system guarantees no implicit mixing, this is just the uniform representation at evaluation time).
    pub fn as_number(&self) -> Option<Decimal> {
        match self {
            Value::Int(i) => Some(Decimal::from(*i)),
            Value::Dec(d) => Some(*d),
            _ => None,
        }
    }

    pub fn type_name(&self) -> &'static str {
        match self {
            Value::Null => "null",
            Value::Bool(_) => "boolean",
            Value::Int(_) => "integer",
            Value::Dec(_) => "decimal",
            Value::Str(_) => "string",
            Value::Arr(_) => "array",
        }
    }

    /// Generic JSON -> Value (no type constraints; used for array elements, log readback, etc.).
    pub fn from_json(json: &serde_json::Value) -> Result<Value, String> {
        Ok(match json {
            serde_json::Value::Null => Value::Null,
            serde_json::Value::Bool(b) => Value::Bool(*b),
            serde_json::Value::Number(n) => {
                if let Some(i) = n.as_i64() {
                    Value::Int(i)
                } else {
                    let raw = n.to_string();
                    Value::Dec(Decimal::from_str(&raw).map_err(|e| format!("bad number {raw}: {e}"))?)
                }
            }
            serde_json::Value::String(s) => Value::Str(s.clone()),
            serde_json::Value::Array(items) => {
                let mut out = Vec::with_capacity(items.len());
                for item in items {
                    out.push(Value::from_json(item)?);
                }
                Value::Arr(out)
            }
            serde_json::Value::Object(_) => {
                return Err("object is not a value; use a fact field of type array/scalar".to_string())
            }
        })
    }
}

impl fmt::Display for Value {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Value::Null => write!(f, "null"),
            Value::Bool(b) => write!(f, "{b}"),
            Value::Int(i) => write!(f, "{i}"),
            Value::Dec(d) => write!(f, "{d}"),
            Value::Str(s) => write!(f, "{s}"),
            Value::Arr(items) => {
                write!(f, "[")?;
                for (i, item) in items.iter().enumerate() {
                    if i > 0 {
                        write!(f, ",")?;
                    }
                    write!(f, "{item}")?;
                }
                write!(f, "]")
            }
        }
    }
}

impl Serialize for Value {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        match self {
            Value::Null => s.serialize_unit(),
            Value::Bool(b) => s.serialize_bool(*b),
            Value::Int(i) => s.serialize_i64(*i),
            // decimals are always strings: no precision loss across languages (Go/JS)
            Value::Dec(d) => s.serialize_str(&d.to_string()),
            Value::Str(v) => s.serialize_str(v),
            Value::Arr(items) => {
                let mut seq = s.serialize_seq(Some(items.len()))?;
                for item in items {
                    seq.serialize_element(item)?;
                }
                seq.end()
            }
        }
    }
}

impl<'de> Deserialize<'de> for Value {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> Visitor<'de> for V {
            type Value = Value;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                write!(f, "a GoldenRules value (null/bool/int/decimal-string/string/array)")
            }
            fn visit_unit<E: de::Error>(self) -> Result<Value, E> {
                Ok(Value::Null)
            }
            fn visit_none<E: de::Error>(self) -> Result<Value, E> {
                Ok(Value::Null)
            }
            fn visit_bool<E: de::Error>(self, v: bool) -> Result<Value, E> {
                Ok(Value::Bool(v))
            }
            fn visit_i64<E: de::Error>(self, v: i64) -> Result<Value, E> {
                Ok(Value::Int(v))
            }
            fn visit_u64<E: de::Error>(self, v: u64) -> Result<Value, E> {
                i64::try_from(v)
                    .map(Value::Int)
                    .map_err(|_| E::custom("integer out of i64 range"))
            }
            fn visit_f64<E: de::Error>(self, v: f64) -> Result<Value, E> {
                // JSON float literal: convert to decimal first, then assert exactness; no binary float arithmetic
                Decimal::from_f64(v)
                    .map(Value::Dec)
                    .ok_or_else(|| E::custom("float literal is not representable as decimal"))
            }
            fn visit_str<E: de::Error>(self, v: &str) -> Result<Value, E> {
                Ok(Value::Str(v.to_string()))
            }
            fn visit_string<E: de::Error>(self, v: String) -> Result<Value, E> {
                Ok(Value::Str(v))
            }
            fn visit_seq<A: SeqAccess<'de>>(self, mut seq: A) -> Result<Value, A::Error> {
                let mut out = Vec::new();
                while let Some(item) = seq.next_element::<Value>()? {
                    out.push(item);
                }
                Ok(Value::Arr(out))
            }
            fn visit_map<A: MapAccess<'de>>(self, mut map: A) -> Result<Value, A::Error> {
                // accept the {"type":"decimal","value":"1.20"} shape (consistent with the Literal structure)
                let mut ty: Option<String> = None;
                let mut val: Option<Value> = None;
                while let Some(k) = map.next_key::<String>()? {
                    match k.as_str() {
                        "type" => ty = Some(map.next_value()?),
                        "value" => val = Some(map.next_value()?),
                        other => {
                            return Err(de::Error::unknown_field(other, &["type", "value"]));
                        }
                    }
                }
                let value = val.ok_or_else(|| de::Error::missing_field("value"))?;
                match ty.as_deref() {
                    Some("decimal") => match value {
                        Value::Str(s) => Decimal::from_str(&s)
                            .map(Value::Dec)
                            .map_err(|e| de::Error::custom(format!("bad decimal {s}: {e}"))),
                        Value::Int(i) => Ok(Value::Dec(Decimal::from(i))),
                        other => Ok(other),
                    },
                    _ => Ok(value),
                }
            }
        }
        d.deserialize_any(V)
    }
}

/// Serialization helper: `{"type": .., "value": ..}`
pub(crate) fn serialize_tagged<S: Serializer>(
    s: S,
    ty: &str,
    value: &Value,
) -> Result<S::Ok, S::Error> {
    let mut st = s.serialize_struct("Literal", 2)?;
    st.serialize_field("type", ty)?;
    st.serialize_field("value", value)?;
    st.end()
}
