package effect

import (
	"errors"

	"github.com/anggasct/aura/internal/platform/fault"
	"modernc.org/sqlite"
)

const sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeClassificationMissing     ErrorCode = "effect_classification_missing"
	ErrorCodeIdempotencyConflict       ErrorCode = "effect_idempotency_conflict"
	ErrorCodeIdempotencyKeyInUse       ErrorCode = "effect_idempotency_key_in_use"
	ErrorCodeResultUnavailable         ErrorCode = "effect_result_unavailable"
	ErrorCodeResultCorrupt             ErrorCode = "effect_result_corrupt"
	ErrorCodeTransitionInvalid         ErrorCode = "effect_transition_invalid"
	ErrorCodeUnknown                   ErrorCode = "effect_unknown"
	ErrorCodeNotFound                  ErrorCode = "effect_not_found"
	ErrorCodeReconciliationUnsupported ErrorCode = "effect_reconciliation_unsupported"
	ErrorCodeReconciliationFailed      ErrorCode = "effect_reconciliation_failed"
	ErrorCodeRequestUnsafe             ErrorCode = "effect_request_unsafe"
	ErrorCodeRetryApprovalRequired     ErrorCode = "effect_retry_approval_required"
	ErrorCodeApprovalInvalid           ErrorCode = "effect_approval_invalid"
	ErrorCodeApprovalExpired           ErrorCode = "effect_approval_expired"
	ErrorCodeApprovalConsumed          ErrorCode = "effect_approval_consumed"
)

func codedError(code ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

var errIntentNotFound = errors.New("effect: intent not found")

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}
