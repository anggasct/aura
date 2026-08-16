package effect

import (
	"errors"
	"fmt"

	"modernc.org/sqlite"
)

const sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeClassificationMissing     ErrorCode = "effect_classification_missing"
	ErrorCodeIdempotencyConflict       ErrorCode = "effect_idempotency_conflict"
	ErrorCodeTransitionInvalid         ErrorCode = "effect_transition_invalid"
	ErrorCodeUnknown                   ErrorCode = "effect_unknown"
	ErrorCodeNotFound                  ErrorCode = "effect_not_found"
	ErrorCodeReconciliationUnsupported ErrorCode = "effect_reconciliation_unsupported"
	ErrorCodeReconciliationFailed      ErrorCode = "effect_reconciliation_failed"
	ErrorCodeRetryApprovalRequired     ErrorCode = "effect_retry_approval_required"
	ErrorCodeApprovalInvalid           ErrorCode = "effect_approval_invalid"
	ErrorCodeApprovalExpired           ErrorCode = "effect_approval_expired"
	ErrorCodeApprovalConsumed          ErrorCode = "effect_approval_consumed"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func codedError(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &Error{Code: code, Detail: detail}, cause)
}

var errIntentNotFound = errors.New("effect: intent not found")

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}
