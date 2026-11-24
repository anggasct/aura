package sqlite

import (
	"errors"

	sqlitedriver "modernc.org/sqlite"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	sqlitePrimaryMask = 0xff

	sqliteBusy       = 5
	sqliteReadOnly   = 8
	sqliteIOErr      = 10
	sqliteCorrupt    = 11
	sqliteFull       = 13
	sqliteCantOpen   = 14
	sqliteConstraint = 19
	sqliteNotADB     = 26

	sqliteConstraintCheck      = 275
	sqliteConstraintForeignKey = 787
	sqliteConstraintNotNull    = 1299
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

func Classify(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := fault.CodeOf[ErrorCode](err); ok {
		return err
	}
	var driverErr *sqlitedriver.Error
	if !errors.As(err, &driverErr) {
		return err
	}
	code, detail := classifyDriverCode(driverErr.Code())
	return fault.Wrap(code, detail, err)
}

func classifyDriverCode(code int) (mapped ErrorCode, detail string) {
	switch code & sqlitePrimaryMask {
	case sqliteBusy:
		return ErrorCodeBusy, "database is busy"
	case sqliteConstraint:
		switch code {
		case sqliteConstraintUnique:
			return ErrorCodeConstraintUnique, ""
		case sqliteConstraintPrimaryKey:
			return ErrorCodeConstraintPrimaryKey, ""
		case sqliteConstraintForeignKey:
			return ErrorCodeConstraintForeignKey, ""
		case sqliteConstraintCheck:
			return ErrorCodeConstraintCheck, ""
		case sqliteConstraintNotNull:
			return ErrorCodeConstraintNotNull, ""
		default:
			return ErrorCodeConstraint, ""
		}
	case sqliteCorrupt:
		return ErrorCodeCorrupt, ""
	case sqliteReadOnly, sqliteIOErr, sqliteFull, sqliteCantOpen, sqliteNotADB:
		return ErrorCodeUnavailable, ""
	default:
		return ErrorCodeFailed, ""
	}
}
