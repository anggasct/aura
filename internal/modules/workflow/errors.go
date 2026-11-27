package workflow

import "github.com/anggasct/aura/internal/platform/fault"

type ErrorCode string

const (
	ErrorCodeSpecInvalid           ErrorCode = "workflow_spec_invalid"
	ErrorCodeDuplicateStep         ErrorCode = "workflow_duplicate_step"
	ErrorCodeUnknownDependency     ErrorCode = "workflow_unknown_dependency"
	ErrorCodeCycleDetected         ErrorCode = "workflow_cycle_detected"
	ErrorCodeExecutorInvalid       ErrorCode = "workflow_executor_invalid"
	ErrorCodeConditionInvalid      ErrorCode = "workflow_condition_invalid"
	ErrorCodeDangerousUncovered    ErrorCode = "workflow_dangerous_operation_uncovered"
	ErrorCodeRunNotFound           ErrorCode = "workflow_run_not_found"
	ErrorCodeDefinitionNotFound    ErrorCode = "workflow_definition_not_found"
	ErrorCodeStepFailed            ErrorCode = "workflow_step_failed"
	ErrorCodeCorrelationUnmatched  ErrorCode = "workflow_correlation_unmatched"
	ErrorCodeCorrelationConflict   ErrorCode = "workflow_correlation_conflict"
	ErrorCodeApprovalRejected      ErrorCode = "workflow_approval_rejected"
	ErrorCodeStepTimeout           ErrorCode = "workflow_step_timeout"
	ErrorCodeBudgetExhausted       ErrorCode = "workflow_budget_exhausted"
	ErrorCodeConcurrencyBounded    ErrorCode = "workflow_concurrency_bounded"
	ErrorCodeCompositionInvalid    ErrorCode = "workflow_composition_invalid"
	ErrorCodeChildFailed           ErrorCode = "workflow_child_failed"
	ErrorCodeCapabilityExceeded    ErrorCode = "workflow_capability_exceeded"
	ErrorCodeDigestMismatch        ErrorCode = "workflow_proposal_digest_mismatch"
	ErrorCodeProposalNotFound      ErrorCode = "workflow_proposal_not_found"
	ErrorCodeProposalDecided       ErrorCode = "workflow_proposal_decided"
	ErrorCodeNotPermitted          ErrorCode = "workflow_not_permitted"
	ErrorCodeRoutineInvalid        ErrorCode = "workflow_routine_invalid"
	ErrorCodeOriginIncomplete      ErrorCode = "workflow_origin_incomplete"
	ErrorCodeApprovalMissing       ErrorCode = "workflow_approval_missing"
	ErrorCodeApprovalConflict      ErrorCode = "workflow_approval_conflict"
	ErrorCodePlanUnshowable        ErrorCode = "workflow_plan_unshowable"
	ErrorCodePersonaMissing        ErrorCode = "workflow_persona_missing"
	ErrorCodeSessionInvalid        ErrorCode = "workflow_session_invalid"
	ErrorCodeApprovalSpent         ErrorCode = "workflow_approval_spent"
	ErrorCodeAgentOutputInvalid    ErrorCode = "workflow_agent_output_invalid"
	ErrorCodeToolUnavailable       ErrorCode = "workflow_tool_unavailable"
	ErrorCodeInvalidArgument       ErrorCode = "invalid_argument"
	ErrorCodeCapabilityUnavailable ErrorCode = "capability_unavailable"
)

func codedError(code ErrorCode, detail string) error {
	return fault.New(code, detail)
}
