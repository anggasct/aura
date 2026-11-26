package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument           ErrorCode = "invalid_argument"
	ErrorCodeSessionNotFound           ErrorCode = "session_not_found"
	ErrorCodeEventNotFound             ErrorCode = "event_not_found"
	ErrorCodeArtifactQuotaExceeded     ErrorCode = "artifact_quota_exceeded"
	ErrorCodeArtifactBlobMissing       ErrorCode = "artifact_blob_missing"
	ErrorCodeBackupDestinationConflict ErrorCode = "backup_destination_conflict"
	ErrorCodeBackupInvalid             ErrorCode = "backup_invalid"
	ErrorCodeRestoreLocked             ErrorCode = "restore_locked"
	ErrorCodeStorageUnavailable        ErrorCode = "storage_unavailable"
	ErrorCodeWebhookExecutionConflict  ErrorCode = "webhook_execution_conflict"
	ErrorCodeWebhookExecutionNotFound  ErrorCode = "webhook_execution_not_found"
	ErrorCodeWebhookExecutionInvalid   ErrorCode = "webhook_execution_invalid"
	ErrorCodeChannelResumeInvalid      ErrorCode = "channel_resume_invalid"
	ErrorCodeBroadcastConflict         ErrorCode = "broadcast_conflict"
	ErrorCodeBroadcastNotFound         ErrorCode = "broadcast_not_found"
	ErrorCodeBroadcastInvalid          ErrorCode = "broadcast_invalid"
	ErrorCodeScheduleConflict          ErrorCode = "schedule_conflict"
	ErrorCodeScheduleNotFound          ErrorCode = "schedule_not_found"
	ErrorCodeScheduleInvalid           ErrorCode = "schedule_invalid"
	ErrorCodeMemoryConflict            ErrorCode = "memory_conflict"
	ErrorCodeMemoryNotFound            ErrorCode = "memory_not_found"
	ErrorCodeMemoryInvalid             ErrorCode = "memory_invalid"
	ErrorCodeSkillNotFound             ErrorCode = "skill_not_found"
	ErrorCodeSkillInvalid              ErrorCode = "skill_invalid"
	ErrorCodeSkillDigestChanged        ErrorCode = "skill_digest_changed"
	ErrorCodeProfileConflict           ErrorCode = "profile_conflict"
	ErrorCodeProfileNotFound           ErrorCode = "profile_not_found"
	ErrorCodeProfileInvalid            ErrorCode = "profile_invalid"
	ErrorCodeChildConflict             ErrorCode = "child_conflict"
	ErrorCodeChildNotFound             ErrorCode = "child_not_found"
	ErrorCodeChildInvalid              ErrorCode = "child_invalid"
	ErrorCodeAPIKeyConflict            ErrorCode = "apikey_conflict"
	ErrorCodeAPIKeyNotFound            ErrorCode = "apikey_not_found"
	ErrorCodeAPIKeyInvalid             ErrorCode = "apikey_invalid"
	ErrorCodePluginConflict            ErrorCode = "plugin_conflict"
	ErrorCodePluginNotFound            ErrorCode = "plugin_not_found"
	ErrorCodePluginInvalid             ErrorCode = "plugin_invalid"
	ErrorCodeErasureBlocked            ErrorCode = "erasure_blocked_by_live_runs"
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

type rowChecker interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func rowExists(ctx context.Context, q rowChecker, query string, args ...any) bool {
	var one int
	return q.QueryRowContext(ctx, query, args...).Scan(&one) == nil
}

func classifyFKReference(ctx context.Context, q rowChecker, refs ...fkReference) error {
	for _, ref := range refs {
		if !rowExists(ctx, q, ref.existsSQL, ref.key) {
			return fault.New(ref.code, fmt.Sprintf("%s %s does not exist", ref.what, ref.key))
		}
	}
	return nil
}

type fkReference struct {
	what      string
	key       any
	existsSQL string
	code      ErrorCode
}
