package turn

import "github.com/anggasct/aura/internal/platform/fault"

type ErrorCode string

const (
	ErrorCodeRuntimeOverloaded          ErrorCode = "runtime_overloaded"
	ErrorCodeTurnDeadlineExceeded       ErrorCode = "turn_deadline_exceeded"
	ErrorCodeTurnCancelled              ErrorCode = "turn_cancelled"
	ErrorCodeModelCapabilityUnsupported ErrorCode = "model_capability_unsupported"
	ErrorCodePolicyDenied               ErrorCode = "policy_denied"
	ErrorCodeStorageUnavailable         ErrorCode = "storage_unavailable"
	ErrorCodeBudgetExhausted            ErrorCode = "budget_exhausted"
	ErrorCodeRuntimeInternal            ErrorCode = "runtime_internal"
	ErrorCodeInvalidArgument            ErrorCode = "invalid_argument"
	ErrorCodeDeliveryFailed             ErrorCode = "delivery_failed"
	ErrorCodeDeliveryAmbiguous          ErrorCode = "delivery_ambiguous"
	ErrorCodeDeliverySkipped            ErrorCode = "delivery_skipped"
	ErrorCodeCheckpointInvalid          ErrorCode = "checkpoint_invalid"
	ErrorCodeCheckpointNotFound         ErrorCode = "checkpoint_not_found"
	ErrorCodeCheckpointStale            ErrorCode = "checkpoint_stale"
	ErrorCodeCheckpointUnsupported      ErrorCode = "checkpoint_unsupported"
	ErrorCodePersonaNotFound            ErrorCode = "persona_not_found"
	ErrorCodePersonaMismatch            ErrorCode = "persona_mismatch"
	ErrorCodeSkillUnavailable           ErrorCode = "skill_unavailable"
)

func (c ErrorCode) Retryable() bool {
	switch c {
	case ErrorCodeRuntimeOverloaded, ErrorCodeStorageUnavailable:
		return true
	default:
		return false
	}
}

func codedError(code ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

func invalidArgument(detail string) error {
	return fault.New(ErrorCodeInvalidArgument, detail)
}
