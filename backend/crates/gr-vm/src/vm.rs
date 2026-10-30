//! Stack-based VM (`contracts/01` §4.3).
//!
//! Semantic boundary (A-7): no loops, no side effects, whitelisted operators. Execution is therefore "one match per instruction",
//! and the only control flow is logical short-circuit jumps. The budget is charged per instruction; going over is `EXEC_STEP_LIMIT` (the base for B11).

use crate::compile::{BinKind, Chunk, Instr};
use gr_contract::{ErrorCode, ErrorContract, Func, Value};
use rust_decimal::prelude::ToPrimitive;
use rust_decimal::{Decimal, RoundingStrategy};
use std::cmp::Ordering;
use std::str::FromStr;

/// Default division precision (`contracts/01` §3.3: "division specifies precision, 8 digits by default, round half up")
pub const DIV_SCALE: u32 = 8;

#[derive(Clone, Debug)]
pub struct VmError {
    pub code: ErrorCode,
    pub message: String,
}

impl VmError {
    pub fn type_error(msg: impl Into<String>) -> Self {
        Self { code: ErrorCode::ContractTypeError, message: msg.into() }
    }

    pub fn contract(msg: impl Into<String>) -> Self {
        Self { code: ErrorCode::ContractViolation, message: msg.into() }
    }
}

impl From<VmError> for ErrorContract {
    fn from(e: VmError) -> Self {
        ErrorContract::new(e.code, e.message)
    }
}

/// Execution budget: one per decision, charged per instruction.
#[derive(Clone, Debug)]
pub struct Budget {
    pub steps_left: u64,
    pub steps_used: u64,
}

impl Budget {
    pub fn new(steps: u64) -> Self {
        Self { steps_left: steps, steps_used: 0 }
    }

    fn tick(&mut self) -> Result<(), VmError> {
        if self.steps_left == 0 {
            return Err(VmError {
                code: ErrorCode::ExecStepLimit,
                message: "执行预算耗尽（单次决策指令数超限）".to_string(),
            });
        }
        self.steps_left -= 1;
        self.steps_used += 1;
        Ok(())
    }
}

/// Execute one chunk and return the stack top value.
pub fn run(
    chunk: &Chunk,
    slots: &[Value],
    request_time: &str,
    budget: &mut Budget,
) -> Result<Value, VmError> {
    let mut stack: Vec<Value> = Vec::with_capacity(16);
    let mut pc: usize = 0;

    while pc < chunk.code.len() {
        budget.tick()?;
        let instr = chunk.code[pc];
        pc += 1;
        match instr {
            Instr::PushConst(i) => stack.push(chunk.consts[i as usize].clone()),
            Instr::LoadSlot(s) => {
                let v = slots
                    .get(s as usize)
                    .ok_or_else(|| VmError::contract(format!("slot {s} 越界")))?;
                stack.push(v.clone());
            }
            Instr::Pop => {
                stack.pop();
            }
            Instr::PushBool(b) => stack.push(Value::Bool(b)),
            Instr::Not => {
                let v = pop(&mut stack)?;
                stack.push(match v {
                    Value::Null => Value::Null,
                    other => Value::Bool(!other.is_truthy()),
                });
            }
            Instr::IsNull { negated } => {
                let v = pop(&mut stack)?;
                let is_null = v.is_null();
                stack.push(Value::Bool(if negated { !is_null } else { is_null }));
            }
            Instr::Jump(t) => pc = t as usize,
            Instr::JumpIfFalse(t) => {
                let v = pop(&mut stack)?;
                if !v.is_truthy() {
                    pc = t as usize;
                }
            }
            Instr::JumpIfTrue(t) => {
                let v = pop(&mut stack)?;
                if v.is_truthy() {
                    pc = t as usize;
                }
            }
            Instr::Bin(kind) => {
                let right = pop(&mut stack)?;
                let left = pop(&mut stack)?;
                stack.push(binary(kind, left, right)?);
            }
            Instr::Call { func, argc } => {
                let argc = argc as usize;
                if stack.len() < argc {
                    return Err(VmError::contract("函数调用栈下溢"));
                }
                // the stack holds arg0..argN from bottom to top, split_off preserves that order
                let args: Vec<Value> = stack.split_off(stack.len() - argc);
                stack.push(call(func, &args, request_time)?);
            }
            Instr::Return => return pop(&mut stack),
        }
    }
    // no explicit Return: take the stack top
    pop(&mut stack)
}

fn pop(stack: &mut Vec<Value>) -> Result<Value, VmError> {
    stack.pop().ok_or_else(|| VmError::contract("求值栈下溢"))
}

fn binary(kind: BinKind, left: Value, right: Value) -> Result<Value, VmError> {
    use BinKind::*;
    // three-valued logic: null in a comparison/arithmetic -> null (`contracts/01` §3.4)
    if matches!(kind, Eq | Ne | Gt | Gte | Lt | Lte | Add | Sub | Mul | Div | Mod) {
        if left.is_null() || right.is_null() {
            return Ok(Value::Null);
        }
    }

    match kind {
        Eq => Ok(Value::Bool(values_equal(&left, &right))),
        Ne => Ok(Value::Bool(!values_equal(&left, &right))),
        Gt | Gte | Lt | Lte => {
            let ord = compare(&left, &right)?;
            Ok(Value::Bool(match kind {
                Gt => ord == Ordering::Greater,
                Gte => ord != Ordering::Less,
                Lt => ord == Ordering::Less,
                _ => ord != Ordering::Greater,
            }))
        }
        Add | Sub | Mul | Div | Mod => arithmetic(kind, left, right),
        In | NotIn => {
            let list = match right {
                Value::Arr(items) => items,
                other => {
                    return Err(VmError::type_error(format!(
                        "in 运算的右操作数必须是数组，实际 {}",
                        other.type_name()
                    )))
                }
            };
            let hit = list.iter().any(|item| values_equal(&left, item));
            Ok(Value::Bool(if matches!(kind, NotIn) { !hit } else { hit }))
        }
    }
}

fn values_equal(a: &Value, b: &Value) -> bool {
    match (a.as_number(), b.as_number()) {
        (Some(x), Some(y)) => x == y,
        _ => a == b,
    }
}

fn compare(a: &Value, b: &Value) -> Result<Ordering, VmError> {
    match (a, b) {
        (Value::Str(x), Value::Str(y)) => Ok(x.cmp(y)),
        (Value::Bool(x), Value::Bool(y)) => Ok(x.cmp(y)),
        _ => match (a.as_number(), b.as_number()) {
            (Some(x), Some(y)) => Ok(x.cmp(&y)),
            _ => Err(VmError::type_error(format!(
                "不可比较的类型：{} vs {}",
                a.type_name(),
                b.type_name()
            ))),
        },
    }
}

fn arithmetic(kind: BinKind, left: Value, right: Value) -> Result<Value, VmError> {
    let both_int = matches!(left, Value::Int(_)) && matches!(right, Value::Int(_));
    let (a, b) = match (left.as_number(), right.as_number()) {
        (Some(a), Some(b)) => (a, b),
        _ => {
            return Err(VmError::type_error(format!(
                "算术运算需要数值：{} vs {}",
                left.type_name(),
                right.type_name()
            )))
        }
    };

    let result = match kind {
        BinKind::Add => a.checked_add(b),
        BinKind::Sub => a.checked_sub(b),
        BinKind::Mul => a.checked_mul(b),
        BinKind::Div => {
            if b.is_zero() {
                return Err(VmError::contract("除以零"));
            }
            a.checked_div(b)
        }
        BinKind::Mod => {
            if b.is_zero() {
                return Err(VmError::contract("取模零"));
            }
            a.checked_rem(b)
        }
        _ => unreachable!("non-arithmetic bin op"),
    }
    .ok_or_else(|| VmError::contract("算术溢出（decimal 超出 96 位尾数）"))?;

    let result = if matches!(kind, BinKind::Div) {
        result.round_dp_with_strategy(DIV_SCALE, RoundingStrategy::MidpointAwayFromZero)
    } else {
        result
    };

    // integer in, integer out: keep 1+1 from becoming "2" (a string) and confusing downstream type checks
    if both_int && !matches!(kind, BinKind::Div) {
        if let Some(i) = result.to_i64() {
            if result.fract().is_zero() {
                return Ok(Value::Int(i));
            }
        }
    }
    Ok(Value::Dec(result))
}

fn call(func: Func, args: &[Value], request_time: &str) -> Result<Value, VmError> {
    match func {
        Func::Now => Ok(Value::Str(request_time.to_string())),
        Func::IsNull => Ok(Value::Bool(args[0].is_null())),
        Func::Coalesce => Ok(args
            .iter()
            .find(|v| !v.is_null())
            .cloned()
            .unwrap_or(Value::Null)),
        Func::InSet => match &args[1] {
            Value::Arr(items) => Ok(Value::Bool(items.iter().any(|i| values_equal(&args[0], i)))),
            other => Err(VmError::type_error(format!(
                "inSet 第二参数必须是数组，实际 {}",
                other.type_name()
            ))),
        },
        Func::Contains => match &args[0] {
            Value::Arr(items) => Ok(Value::Bool(items.iter().any(|i| values_equal(i, &args[1])))),
            other => Err(VmError::type_error(format!(
                "contains 第一参数必须是数组，实际 {}",
                other.type_name()
            ))),
        },
        Func::Abs => {
            let n = num(&args[0], "abs")?;
            Ok(numeric(n.abs(), &args[0]))
        }
        Func::Min | Func::Max => {
            let mut best: Option<Decimal> = None;
            for a in args {
                let n = num(a, func.name())?;
                best = Some(match (best, func) {
                    (None, _) => n,
                    (Some(b), Func::Min) => b.min(n),
                    (Some(b), _) => b.max(n),
                });
            }
            let best = best.ok_or_else(|| VmError::contract("min/max 需要至少一个参数"))?;
            Ok(numeric(best, &args[0]))
        }
        Func::Round => {
            let n = num(&args[0], "round")?;
            let scale = match args.get(1) {
                Some(v) => num(v, "round")?
                    .to_u32()
                    .ok_or_else(|| VmError::type_error("round 精度必须是非负整数"))?,
                None => 0,
            };
            Ok(Value::Dec(n.round_dp_with_strategy(scale, RoundingStrategy::MidpointAwayFromZero)))
        }
        Func::StringLen => match &args[0] {
            Value::Str(s) => Ok(Value::Int(s.chars().count() as i64)),
            other => Err(VmError::type_error(format!(
                "stringLen 需要字符串，实际 {}",
                other.type_name()
            ))),
        },
        Func::ToUpper | Func::ToLower => match &args[0] {
            Value::Str(s) => Ok(Value::Str(if matches!(func, Func::ToUpper) {
                s.to_uppercase()
            } else {
                s.to_lowercase()
            })),
            other => Err(VmError::type_error(format!(
                "{} 需要字符串，实际 {}",
                func.name(),
                other.type_name()
            ))),
        },
        Func::Substr => match &args[0] {
            Value::Str(s) => {
                let chars: Vec<char> = s.chars().collect();
                let start = num(&args[1], "substr")?
                    .to_usize()
                    .ok_or_else(|| VmError::type_error("substr 起点必须是非负整数"))?;
                if start >= chars.len() {
                    return Ok(Value::Str(String::new()));
                }
                let take = match args.get(2) {
                    Some(v) => num(v, "substr")?
                        .to_usize()
                        .ok_or_else(|| VmError::type_error("substr 长度必须是非负整数"))?,
                    None => chars.len() - start,
                };
                let end = (start + take).min(chars.len());
                Ok(Value::Str(chars[start..end].iter().collect()))
            }
            other => Err(VmError::type_error(format!(
                "substr 需要字符串，实际 {}",
                other.type_name()
            ))),
        },
    }
}

fn num(v: &Value, what: &str) -> Result<Decimal, VmError> {
    v.as_number()
        .ok_or_else(|| VmError::type_error(format!("{what} 需要数值，实际 {}", v.type_name())))
}

fn numeric(d: Decimal, original: &Value) -> Value {
    if let Value::Int(_) = original {
        if d.fract().is_zero() {
            if let Some(i) = d.to_i64() {
                return Value::Int(i);
            }
        }
    }
    Value::Dec(d)
}

/// Convenience: parse a literal string into a decimal (reused by the compiler numeric sugar)
pub fn parse_decimal(s: &str) -> Option<Decimal> {
    Decimal::from_str(s).ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::compile::Chunk;

    fn chunk(code: Vec<Instr>, consts: Vec<Value>) -> Chunk {
        Chunk { code, consts }
    }

    #[test]
    fn decimal_division_is_rounded_to_scale_8() {
        let c = chunk(
            vec![
                Instr::PushConst(0),
                Instr::PushConst(1),
                Instr::Bin(BinKind::Div),
                Instr::Return,
            ],
            vec![Value::Dec(Decimal::from(1)), Value::Dec(Decimal::from(3))],
        );
        let mut budget = Budget::new(100);
        let v = run(&c, &[], "2026-10-30T10:00:00Z", &mut budget).unwrap();
        assert_eq!(v.to_string(), "0.33333333");
    }

    #[test]
    fn integer_arithmetic_stays_integer() {
        let c = chunk(
            vec![
                Instr::PushConst(0),
                Instr::PushConst(1),
                Instr::Bin(BinKind::Add),
                Instr::Return,
            ],
            vec![Value::Int(1), Value::Int(2)],
        );
        let mut budget = Budget::new(100);
        assert_eq!(run(&c, &[], "", &mut budget).unwrap(), Value::Int(3));
    }

    #[test]
    fn null_propagates_through_comparison() {
        let c = chunk(
            vec![
                Instr::PushConst(0),
                Instr::PushConst(1),
                Instr::Bin(BinKind::Gt),
                Instr::Return,
            ],
            vec![Value::Null, Value::Int(1)],
        );
        let mut budget = Budget::new(100);
        assert!(run(&c, &[], "", &mut budget).unwrap().is_null());
    }

    #[test]
    fn budget_exhaustion_is_reported() {
        let c = chunk(
            vec![Instr::PushConst(0), Instr::PushConst(0), Instr::Bin(BinKind::Add), Instr::Return],
            vec![Value::Int(1)],
        );
        let mut budget = Budget::new(2);
        let err = run(&c, &[], "", &mut budget).unwrap_err();
        assert_eq!(err.code, ErrorCode::ExecStepLimit);
    }
}
