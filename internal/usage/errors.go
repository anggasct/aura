package usage

import (
	"errors"
	"fmt"

	"modernc.org/sqlite"
)

const sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE

type ErrorCode string

const (
	ErrorCodeInvalidArgument     ErrorCode = "invalid_argument"
	ErrorCodePriceNotFound       ErrorCode = "usage_price_not_found"
	ErrorCodePriceVersionInvalid ErrorCode = "usage_price_version_invalid"
	ErrorCodeReservationConflict ErrorCode = "usage_reservation_conflict"
	ErrorCodeBudgetExceeded      ErrorCode = "usage_budget_exceeded"
	ErrorCodeReservationNotFound ErrorCode = "usage_reservation_not_found"
	ErrorCodeUsageMismatch       ErrorCode = "usage_mismatch"
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

var (
	ErrBudgetExceeded = errors.New("usage: budget exceeded")
	ErrPriceNotFound  = errors.New("usage: no applicable price record")
)

func codedError(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &Error{Code: code, Detail: detail}, cause)
}

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}
