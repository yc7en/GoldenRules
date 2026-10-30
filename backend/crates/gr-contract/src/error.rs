//! Error contract (`contracts/05` §1 / §3)
//!
//! Highest-priority boundary: **a contract violation lands as a REJECT decision, not as an HTTP 500**;
//! only service-level failures (timeout/overload/internal bug) are real errors.

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fmt;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum ErrorCode {
    // --- Contract violations (land as a REJECT decision) ---
    ContractViolation,
    ContractTypeError,
    ContractRequiredMissing,
    ContractUnknownField,
    ContractSnapshotTooLarge,
    ContractEnumInvalid,
    ContractContextSnapshotMismatch,
    // --- Version selection ---
    VersionSelectionConflict,
    VersionRulesetNotFound,
    VersionPackageNotFound,
    // --- Execution ---
    ExecTimeout,
    ExecStepLimit,
    ExecOverload,
    ExecInternal,
    ExecRuleLimitExceeded,
    // --- Rule package / IR ---
    IrInvalid,
    IrUnknownSlot,
    IrDuplicateSlot,
    IrUnknownFact,
    IrVersionUnsupported,
    DslOpNotAllowed,
    DslFnNotFound,
    DslTypeMismatch,
    DslArityMismatch,
    DslDepthLimit,
    DslNodeLimit,
    // --- Output validation ---
    OutputSchemaViolation,
    // --- Provider / Assemble (entry layer, never the execution core) ---
    ProviderUnavailable,
    ProviderTimeout,
    ProviderError,
    AssembleSceneUnknown,
}

impl ErrorCode {
    pub fn as_str(&self) -> &'static str {
        use ErrorCode::*;
        match self {
            ContractViolation => "CONTRACT_VIOLATION",
            ContractTypeError => "CONTRACT_TYPE_ERROR",
            ContractRequiredMissing => "CONTRACT_REQUIRED_MISSING",
            ContractUnknownField => "CONTRACT_UNKNOWN_FIELD",
            ContractSnapshotTooLarge => "CONTRACT_SNAPSHOT_TOO_LARGE",
            ContractEnumInvalid => "CONTRACT_ENUM_INVALID",
            ContractContextSnapshotMismatch => "CONTEXT_SNAPSHOT_MISMATCH",
            VersionSelectionConflict => "VERSION_SELECTION_CONFLICT",
            VersionRulesetNotFound => "VERSION_RULESET_NOT_FOUND",
            VersionPackageNotFound => "VERSION_PACKAGE_NOT_FOUND",
            ExecTimeout => "EXEC_TIMEOUT",
            ExecStepLimit => "EXEC_STEP_LIMIT",
            ExecOverload => "EXEC_OVERLOAD",
            ExecInternal => "EXEC_INTERNAL",
            ExecRuleLimitExceeded => "EXEC_RULE_LIMIT_EXCEEDED",
            IrInvalid => "IR_INVALID",
            IrUnknownSlot => "IR_UNKNOWN_SLOT",
            IrDuplicateSlot => "IR_DUPLICATE_SLOT",
            IrUnknownFact => "IR_UNKNOWN_FACT",
            IrVersionUnsupported => "IR_VERSION_UNSUPPORTED",
            DslOpNotAllowed => "DSL_OP_NOT_ALLOWED",
            DslFnNotFound => "DSL_FN_NOT_FOUND",
            DslTypeMismatch => "DSL_TYPE_MISMATCH",
            DslArityMismatch => "DSL_ARITY_MISMATCH",
            DslDepthLimit => "DSL_DEPTH_LIMIT",
            DslNodeLimit => "DSL_NODE_LIMIT",
            OutputSchemaViolation => "OUTPUT_SCHEMA_VIOLATION",
            ProviderUnavailable => "PROVIDER_UNAVAILABLE",
            ProviderTimeout => "PROVIDER_TIMEOUT",
            ProviderError => "PROVIDER_ERROR",
            AssembleSceneUnknown => "ASSEMBLE_SCENE_UNKNOWN",
        }
    }

    /// Contract violation -> decision; everything else -> a real error.
    pub fn is_contract_violation(&self) -> bool {
        use ErrorCode::*;
        matches!(
            self,
            ContractTypeError
                | ContractRequiredMissing
                | ContractUnknownField
                | ContractSnapshotTooLarge
                | ContractEnumInvalid
                | ContractContextSnapshotMismatch
        )
    }

    /// HTTP status mapping: contract violations are not here (they never return as errors).
    pub fn http_status(&self) -> u16 {
        use ErrorCode::*;
        match self {
            ExecTimeout | ExecOverload | ProviderTimeout | ProviderUnavailable => 503,
            VersionRulesetNotFound | VersionPackageNotFound | AssembleSceneUnknown => 404,
            VersionSelectionConflict => 409,
            _ => 500,
        }
    }
}

impl fmt::Display for ErrorCode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Unified error response (`contracts/05` §3.2)
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ErrorContract {
    pub error_code: ErrorCode,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub trace_id: Option<String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub details: BTreeMap<String, serde_json::Value>,
}

impl ErrorContract {
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        Self { error_code: code, message: message.into(), trace_id: None, details: BTreeMap::new() }
    }

    pub fn with_detail(mut self, key: &str, value: serde_json::Value) -> Self {
        self.details.insert(key.to_string(), value);
        self
    }

    pub fn with_trace(mut self, trace_id: impl Into<String>) -> Self {
        self.trace_id = Some(trace_id.into());
        self
    }
}

impl fmt::Display for ErrorContract {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "[{}] {}", self.error_code, self.message)
    }
}

impl std::error::Error for ErrorContract {}

pub type EngineResult<T> = Result<T, ErrorContract>;

/// One contract violation point: when it lands in a decision it goes into `degradedReasons` and is also recorded as a metric.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ContractIssue {
    pub code: ErrorCode,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fact: Option<String>,
    pub detail: String,
}

impl ContractIssue {
    pub fn new(code: ErrorCode, detail: impl Into<String>) -> Self {
        Self { code, fact: None, detail: detail.into() }
    }

    pub fn with_fact(mut self, fact: impl Into<String>) -> Self {
        self.fact = Some(fact.into());
        self
    }
}
