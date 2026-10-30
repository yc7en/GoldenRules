//! IR expression nodes (`contracts/01` §4.2)
//!
//! All three input shapes converge here:
//! - `{ "type": "decimal", "value": "1000" }` → [`Expr::Literal`]
//! - `{ "slot": 3 }` -> [`Expr::Slot`] (compiled IR; at runtime only slots are recognized)
//! - `{ "fact": "request.amount" }` -> [`Expr::Fact`] (DSL shape, resolved to a slot at load time)
//! - `{ "op": "gt", "args": [..] }` → [`Expr::Op`]
//!
//! Operators are a **whitelist of strings**, checked by `gr-vm` at compile time (`DSL_OP_NOT_ALLOWED`);
//! this layer does not reject unknown ops, so the contract type expresses structure only, never policy.

use crate::ty::Literal;
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum Expr {
    Literal(Literal),
    Slot(SlotRef),
    Fact(FactRef),
    Op(OpExpr),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct SlotRef {
    pub slot: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct FactRef {
    pub fact: String,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct OpExpr {
    pub op: String,
    #[serde(default)]
    pub args: Vec<Expr>,
    /// Used by `isNull` only; ignored by every other op.
    #[serde(default, skip_serializing_if = "is_false")]
    pub negated: bool,
    /// Used only when `op=call`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub func: Option<String>,
}

fn is_false(v: &bool) -> bool {
    !*v
}

impl OpExpr {
    pub fn new(op: &str, args: Vec<Expr>) -> Self {
        Self { op: op.to_string(), args, negated: false, func: None }
    }

    pub fn call(func: &str, args: Vec<Expr>) -> Self {
        Self { op: "call".to_string(), args, negated: false, func: Some(func.to_string()) }
    }
}

impl Expr {
    pub fn slot(idx: u32) -> Expr {
        Expr::Slot(SlotRef { slot: idx })
    }

    pub fn fact(name: &str) -> Expr {
        Expr::Fact(FactRef { fact: name.to_string() })
    }

    pub fn op(op: &str, args: Vec<Expr>) -> Expr {
        Expr::Op(OpExpr::new(op, args))
    }

    pub fn depth(&self) -> usize {
        match self {
            Expr::Literal(_) | Expr::Slot(_) | Expr::Fact(_) => 1,
            Expr::Op(o) => 1 + o.args.iter().map(|a| a.depth()).max().unwrap_or(0),
        }
    }

    pub fn node_count(&self) -> usize {
        match self {
            Expr::Literal(_) | Expr::Slot(_) | Expr::Fact(_) => 1,
            Expr::Op(o) => 1 + o.args.iter().map(|a| a.node_count()).sum::<usize>(),
        }
    }

    /// Collect the slots referenced by this expression (ascending, deduplicated).
    pub fn collect_slots(&self, out: &mut Vec<u32>) {
        match self {
            Expr::Slot(s) => out.push(s.slot),
            Expr::Op(o) => {
                for a in &o.args {
                    a.collect_slots(out);
                }
            }
            _ => {}
        }
    }

    pub fn visit_op<F: FnMut(&OpExpr)>(&self, f: &mut F) {
        if let Expr::Op(o) = self {
            f(o);
            for a in &o.args {
                a.visit_op(f);
            }
        }
    }
}

/// Whitelisted operators (the single place they are decided, at compile time).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Op {
    And,
    Or,
    Not,
    Eq,
    Ne,
    Gt,
    Gte,
    Lt,
    Lte,
    Add,
    Sub,
    Mul,
    Div,
    Mod,
    In,
    NotIn,
    IsNull,
    Call,
}

impl Op {
    pub fn parse(s: &str) -> Option<Op> {
        Some(match s {
            "and" => Op::And,
            "or" => Op::Or,
            "not" => Op::Not,
            "eq" => Op::Eq,
            "ne" => Op::Ne,
            "gt" => Op::Gt,
            "gte" => Op::Gte,
            "lt" => Op::Lt,
            "lte" => Op::Lte,
            "add" => Op::Add,
            "sub" => Op::Sub,
            "mul" => Op::Mul,
            "div" => Op::Div,
            "mod" => Op::Mod,
            "in" => Op::In,
            "notIn" => Op::NotIn,
            "isNull" => Op::IsNull,
            "call" => Op::Call,
            _ => return None,
        })
    }

    pub fn is_comparison(&self) -> bool {
        matches!(self, Op::Eq | Op::Ne | Op::Gt | Op::Gte | Op::Lt | Op::Lte)
    }
}

/// Whitelisted functions (`contracts/01` §3.6 / appendix B).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Func {
    Now,
    InSet,
    Abs,
    Min,
    Max,
    Round,
    Contains,
    StringLen,
    ToUpper,
    ToLower,
    Substr,
    IsNull,
    Coalesce,
}

impl Func {
    pub fn parse(s: &str) -> Option<Func> {
        Some(match s {
            "now" => Func::Now,
            "inSet" => Func::InSet,
            "abs" => Func::Abs,
            "min" => Func::Min,
            "max" => Func::Max,
            "round" => Func::Round,
            "contains" => Func::Contains,
            "stringLen" => Func::StringLen,
            "toUpper" => Func::ToUpper,
            "toLower" => Func::ToLower,
            "substr" => Func::Substr,
            "isNull" => Func::IsNull,
            "coalesce" => Func::Coalesce,
            _ => return None,
        })
    }

    pub fn name(&self) -> &'static str {
        match self {
            Func::Now => "now",
            Func::InSet => "inSet",
            Func::Abs => "abs",
            Func::Min => "min",
            Func::Max => "max",
            Func::Round => "round",
            Func::Contains => "contains",
            Func::StringLen => "stringLen",
            Func::ToUpper => "toUpper",
            Func::ToLower => "toLower",
            Func::Substr => "substr",
            Func::IsNull => "isNull",
            Func::Coalesce => "coalesce",
        }
    }

    /// Legal arity range (both ends inclusive).
    pub fn arity(&self) -> (usize, usize) {
        match self {
            Func::Now => (0, 0),
            Func::InSet => (2, 2),
            Func::Abs => (1, 1),
            Func::Min | Func::Max => (1, usize::MAX),
            Func::Round => (1, 2),
            Func::Contains => (2, 2),
            Func::StringLen | Func::ToUpper | Func::ToLower => (1, 1),
            Func::Substr => (2, 3),
            Func::IsNull => (1, 1),
            // coalesce(a, b, ..): fallback
            Func::Coalesce => (1, usize::MAX),
        }
    }
}
