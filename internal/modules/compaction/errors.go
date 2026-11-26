package compaction

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeCapabilityMissing  ErrorCode = "capability_metadata_missing"
	ErrorCodeBudgetExceeded     ErrorCode = "context_budget_exceeded"
	ErrorCodeSummaryInvalid     ErrorCode = "summary_invalid"
	ErrorCodeSummaryStale       ErrorCode = "summary_stale"
	ErrorCodeSummaryUnavailable ErrorCode = "summary_unavailable"
)
