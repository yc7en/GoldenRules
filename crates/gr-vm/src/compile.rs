//! 字节码定义与 IR→字节码编译器（D3：IR + 字节码 VM，不是纯解释）。
//!
//! 为什么编译：千条规则 / 万级 QPS 下，每次决策重复遍历表达式树要反复做
//! 「判 op 字符串 → 分派」；编译成扁平指令后，执行是一条 `match` 循环。
//!
//! 指令集刻意保持极小（无循环、无跳转表、无函数调用栈），因为 DSL 语义有界（A-7）：
//! 没有循环，唯一的跳转来源是 `and`/`or` 短路。

use gr_contract::{ErrorCode, ErrorContract, Expr, Func, Literal, Op, Value};
use gr_ir::{LoadedRuleSet, LoadedRule};
use rust_decimal::Decimal;
use std::collections::BTreeMap;
use std::str::FromStr;
use std::sync::Arc;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BinKind {
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
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Instr {
    PushConst(u32),
    LoadSlot(u32),
    Pop,
    Not,
    Bin(BinKind),
    IsNull { negated: bool },
    Call { func: Func, argc: u8 },
    Jump(u32),
    /// 弹出栈顶，假则跳转（null 视为假）
    JumpIfFalse(u32),
    /// 弹出栈顶，真则跳转
    JumpIfTrue(u32),
    PushBool(bool),
    Return,
}

#[derive(Clone, Debug, Default)]
pub struct Chunk {
    pub code: Vec<Instr>,
    pub consts: Vec<Value>,
}

impl Chunk {
    pub fn instr_count(&self) -> usize {
        self.code.len()
    }
}

/// 一条规则编译后的产物：`when` 判命中，`assigns` 出输出变量。
#[derive(Clone, Debug)]
pub struct RuleProgram {
    pub rule_index: u32,
    pub rule_id: String,
    pub version: Option<String>,
    pub priority: i64,
    pub reason_code: String,
    pub reason_message: Option<String>,
    pub decision: gr_contract::Decision,
    pub when: Arc<Chunk>,
    /// (变量名, 求值 chunk)
    pub assigns: Vec<(String, Arc<Chunk>)>,
}

/// 一个规则集的编译产物。**顺序与 `LoadedRuleSet.rules` 一致**（已按优先级排好）。
#[derive(Clone, Debug)]
pub struct RuleSetProgram {
    pub ruleset_id: String,
    pub version: String,
    pub scene: String,
    pub hit_strategy: gr_contract::HitStrategy,
    pub default_decision: gr_contract::Decision,
    pub fallback_decision: Option<gr_contract::Decision>,
    pub programs: Vec<Arc<RuleProgram>>,
    pub output_schema_id: Option<String>,
    pub output_schema_version: Option<String>,
    /// 总指令数：用于资源预算与容量评估
    pub total_instrs: usize,
}

impl RuleSetProgram {
    pub fn program(&self, rule_index: u32) -> &Arc<RuleProgram> {
        &self.programs[rule_index as usize]
    }
}

/// 编译整个规则集。任何非白名单 op / 函数 / 参数个数错误都在这里挡住（发布期语义）。
pub fn compile_ruleset(rs: &LoadedRuleSet) -> Result<RuleSetProgram, ErrorContract> {
    let mut programs = Vec::with_capacity(rs.rules.len());
    let mut total = 0usize;
    for rule in &rs.rules {
        let program = compile_rule(&rs.ruleset_id, rule)?;
        total += program.when.instr_count()
            + program.assigns.iter().map(|(_, c)| c.instr_count()).sum::<usize>();
        programs.push(Arc::new(program));
    }
    Ok(RuleSetProgram {
        ruleset_id: rs.ruleset_id.clone(),
        version: rs.version.clone(),
        scene: rs.scene.clone(),
        hit_strategy: rs.hit_strategy,
        default_decision: rs.default_decision.clone(),
        fallback_decision: rs.fallback_decision.clone(),
        programs,
        output_schema_id: rs.output_schema_id.clone(),
        output_schema_version: rs.output_schema_version.clone(),
        total_instrs: total,
    })
}

fn compile_rule(ruleset_id: &str, rule: &LoadedRule) -> Result<RuleProgram, ErrorContract> {
    let mut c = Compiler::new(ruleset_id, &rule.rule.rule_id);
    c.compile(&rule.rule.when)?;
    c.emit(Instr::Return);
    let when = Arc::new(c.finish());

    let mut assigns = Vec::with_capacity(rule.rule.then.assign.len());
    for assign in &rule.rule.then.assign {
        let mut c = Compiler::new(ruleset_id, &rule.rule.rule_id);
        c.compile(&assign.expr)?;
        c.emit(Instr::Return);
        assigns.push((assign.var.clone(), Arc::new(c.finish())));
    }

    Ok(RuleProgram {
        rule_index: rule.index,
        rule_id: rule.rule.rule_id.clone(),
        version: rule.rule.version.clone(),
        priority: rule.rule.priority,
        reason_code: rule.rule.then.reason_code.clone(),
        reason_message: rule.rule.then.reason_message.clone(),
        decision: rule.rule.then.decision.clone(),
        when,
        assigns,
    })
}

struct Compiler<'a> {
    ruleset_id: &'a str,
    rule_id: &'a str,
    code: Vec<Instr>,
    consts: Vec<Value>,
}

impl<'a> Compiler<'a> {
    fn new(ruleset_id: &'a str, rule_id: &'a str) -> Self {
        Self { ruleset_id, rule_id, code: Vec::new(), consts: Vec::new() }
    }

    fn finish(self) -> Chunk {
        Chunk { code: self.code, consts: self.consts }
    }

    fn emit(&mut self, i: Instr) -> usize {
        self.code.push(i);
        self.code.len() - 1
    }

    fn patch(&mut self, at: usize, target: u32) {
        match &mut self.code[at] {
            Instr::Jump(t) | Instr::JumpIfFalse(t) | Instr::JumpIfTrue(t) => *t = target,
            other => panic!("patch on non-jump instruction: {other:?}"),
        }
    }

    fn here(&self) -> u32 {
        self.code.len() as u32
    }

    fn error(&self, code: ErrorCode, detail: String) -> ErrorContract {
        let mut e = ErrorContract::new(code, detail);
        e.details.insert("rulesetId".into(), serde_json::json!(self.ruleset_id));
        e.details.insert("ruleId".into(), serde_json::json!(self.rule_id));
        e
    }

    fn compile(&mut self, expr: &Expr) -> Result<(), ErrorContract> {
        match expr {
            Expr::Literal(l) => {
                let idx = self.consts.len() as u32;
                self.consts.push(l.value.clone());
                self.emit(Instr::PushConst(idx));
                Ok(())
            }
            Expr::Slot(s) => {
                self.emit(Instr::LoadSlot(s.slot));
                Ok(())
            }
            Expr::Fact(f) => Err(self.error(
                ErrorCode::IrInvalid,
                format!("字段引用 {} 未在加载期解析为 slot（应经 gr-ir::load）", f.fact),
            )),
            Expr::Op(op) => self.compile_op(op),
        }
    }

    fn compile_op(&mut self, op: &gr_contract::OpExpr) -> Result<(), ErrorContract> {
        let kind = Op::parse(&op.op).ok_or_else(|| {
            self.error(
                ErrorCode::DslOpNotAllowed,
                format!("运算符 {} 不在白名单（contracts/01 §3.6）", op.op),
            )
        })?;

        match kind {
            Op::And | Op::Or => self.compile_logical(op, kind),
            Op::Not => {
                self.expect_arity(op, 1)?;
                self.compile(&op.args[0])?;
                self.emit(Instr::Not);
                Ok(())
            }
            Op::IsNull => {
                self.expect_arity(op, 1)?;
                self.compile(&op.args[0])?;
                self.emit(Instr::IsNull { negated: op.negated });
                Ok(())
            }
            Op::Call => {
                let fn_name = op.func.clone().ok_or_else(|| {
                    self.error(ErrorCode::DslFnNotFound, "call 缺少 func 字段".to_string())
                })?;
                let func = Func::parse(&fn_name).ok_or_else(|| {
                    self.error(
                        ErrorCode::DslFnNotFound,
                        format!("函数 {fn_name} 不在白名单（contracts/01 §3.6）"),
                    )
                })?;
                let (min, max) = func.arity();
                if op.args.len() < min || op.args.len() > max {
                    return Err(self.error(
                        ErrorCode::DslArityMismatch,
                        format!(
                            "函数 {} 期望 {min}..={max} 个参数，实际 {}",
                            func.name(),
                            op.args.len()
                        ),
                    ));
                }
                for arg in &op.args {
                    self.compile(arg)?;
                }
                self.emit(Instr::Call { func, argc: op.args.len() as u8 });
                Ok(())
            }
            _ => {
                self.expect_arity(op, 2)?;
                // 算术类：允许 "0.1" 这种数值字面量糖（contracts/01 §3.2.3 示例写法）
                let left = self.numeric_sugar(kind, &op.args[0]);
                let right = self.numeric_sugar(kind, &op.args[1]);
                self.compile(&left)?;
                self.compile(&right)?;
                self.emit(Instr::Bin(to_bin(kind)));
                Ok(())
            }
        }
    }

    fn compile_logical(
        &mut self,
        op: &gr_contract::OpExpr,
        kind: Op,
    ) -> Result<(), ErrorContract> {
        if op.args.is_empty() {
            self.emit(Instr::PushBool(kind == Op::And));
            return Ok(());
        }
        if op.args.len() == 1 {
            return self.compile(&op.args[0]);
        }

        let is_and = kind == Op::And;
        let mut exits = Vec::with_capacity(op.args.len() - 1);

        for arg in &op.args[..op.args.len() - 1] {
            self.compile(arg)?;
            let at = self.emit(if is_and {
                Instr::JumpIfFalse(0)
            } else {
                Instr::JumpIfTrue(0)
            });
            exits.push(at);
        }
        self.compile(&op.args[op.args.len() - 1])?;
        let end_jump = self.emit(Instr::Jump(0));

        let short_target = self.here();
        self.emit(Instr::PushBool(!is_and));
        let end = self.here();
        self.patch(end_jump, end);
        for at in exits {
            self.patch(at, short_target);
        }
        Ok(())
    }

    fn expect_arity(&self, op: &gr_contract::OpExpr, want: usize) -> Result<(), ErrorContract> {
        if op.args.len() != want {
            return Err(self.error(
                ErrorCode::DslArityMismatch,
                format!("运算符 {} 期望 {want} 个参数，实际 {}", op.op, op.args.len()),
            ));
        }
        Ok(())
    }

    /// 算术语境下，形如 `"0.1"` 的字符串字面量按 decimal 处理。
    ///
    /// 只在**算术**（add/sub/mul/div/mod）生效，不作用于比较——
    /// 否则枚举/字符串字段和 "1000" 这类字面量比较会被误判。
    fn numeric_sugar(&self, kind: Op, expr: &Expr) -> Expr {
        if !matches!(kind, Op::Add | Op::Sub | Op::Mul | Op::Div | Op::Mod) {
            return expr.clone();
        }
        if let Expr::Literal(l) = expr {
            if l.ty == gr_contract::FactKind::String {
                if let Value::Str(s) = &l.value {
                    if let Ok(d) = Decimal::from_str(s) {
                        return Expr::Literal(Literal::new(gr_contract::FactKind::Decimal, Value::Dec(d)));
                    }
                }
            }
        }
        expr.clone()
    }
}

fn to_bin(kind: Op) -> BinKind {
    match kind {
        Op::Eq => BinKind::Eq,
        Op::Ne => BinKind::Ne,
        Op::Gt => BinKind::Gt,
        Op::Gte => BinKind::Gte,
        Op::Lt => BinKind::Lt,
        Op::Lte => BinKind::Lte,
        Op::Add => BinKind::Add,
        Op::Sub => BinKind::Sub,
        Op::Mul => BinKind::Mul,
        Op::Div => BinKind::Div,
        Op::Mod => BinKind::Mod,
        Op::In => BinKind::In,
        Op::NotIn => BinKind::NotIn,
        other => panic!("to_bin called with non-binary op: {other:?}"),
    }
}

/// 编译一个规则集里的全部规则，返回 (ruleset_id → program) 映射，便于 B3 直接用。
pub fn compile_all(
    loaded: &gr_ir::LoadedPackage,
) -> Result<BTreeMap<String, Arc<RuleSetProgram>>, ErrorContract> {
    let mut out = BTreeMap::new();
    for (id, rs) in &loaded.rulesets {
        out.insert(id.clone(), Arc::new(compile_ruleset(rs)?));
    }
    Ok(out)
}
