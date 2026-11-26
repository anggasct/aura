package store

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument      ErrorCode = "invalid_argument"
	ErrorCodeStorageUnavailable   ErrorCode = "storage_unavailable"
	ErrorCodeChannelResumeInvalid ErrorCode = "channel_resume_invalid"
	ErrorCodeBroadcastConflict    ErrorCode = "broadcast_conflict"
	ErrorCodeBroadcastNotFound    ErrorCode = "broadcast_not_found"
	ErrorCodeBroadcastInvalid     ErrorCode = "broadcast_invalid"
	ErrorCodeScheduleConflict     ErrorCode = "schedule_conflict"
	ErrorCodeScheduleNotFound     ErrorCode = "schedule_not_found"
	ErrorCodeScheduleInvalid      ErrorCode = "schedule_invalid"
	ErrorCodeMemoryConflict       ErrorCode = "memory_conflict"
	ErrorCodeMemoryNotFound       ErrorCode = "memory_not_found"
	ErrorCodeMemoryInvalid        ErrorCode = "memory_invalid"
	ErrorCodeChildConflict        ErrorCode = "child_conflict"
	ErrorCodeChildNotFound        ErrorCode = "child_not_found"
	ErrorCodeChildInvalid         ErrorCode = "child_invalid"
	ErrorCodePluginConflict       ErrorCode = "plugin_conflict"
	ErrorCodePluginNotFound       ErrorCode = "plugin_not_found"
	ErrorCodePluginInvalid        ErrorCode = "plugin_invalid"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func codedError(code ErrorCode, detail string, cause error) error {
	if cause == nil {
		return fault.New(code, detail)
	}
	return fault.Wrap(code, detail, cause)
}

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}

func classify(err error) error {
	return sqlite.Classify(err)
}

func isConstraintUnique(err error) bool {
	classified := sqlite.Classify(err)
	return fault.HasCode(classified, sqlite.ErrorCodeConstraintUnique) ||
		fault.HasCode(classified, sqlite.ErrorCodeConstraintPrimaryKey)
}

func isConstraintForeignKey(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintForeignKey)
}

func isConstraintCheck(err error) bool {
	return fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck)
}
