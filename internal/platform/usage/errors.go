package usage

import (
	"errors"

	"github.com/anggasct/aura/internal/platform/fault"
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

var (
	ErrBudgetExceeded = errors.New("usage: budget exceeded")
	ErrPriceNotFound  = errors.New("usage: no applicable price record")

	errReservationNotFound = errors.New("usage: reservation not found in conflict recovery")

	errSettlementNotFound = errors.New("usage: settlement not found in conflict recovery")
)

func codedError(code ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}
