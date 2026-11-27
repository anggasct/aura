package memory

type ErrorCode string

const (
	ErrorCodeInvalidArgument            ErrorCode = "invalid_argument"
	ErrorCodeQueryInvalid               ErrorCode = "memory_query_invalid"
	ErrorCodeBudgetInvalid              ErrorCode = "memory_budget_invalid"
	ErrorCodeProjectionStale            ErrorCode = "memory_projection_stale"
	ErrorCodeProjectionCorrupt          ErrorCode = "memory_projection_corrupt"
	ErrorCodeModelCapabilityUnsupported ErrorCode = "memory_model_capability_unsupported"
	ErrorCodeRebuildRequired            ErrorCode = "memory_rebuild_required"
	ErrorCodeForbidden                  ErrorCode = "memory_forbidden"
	ErrorCodeUnavailable                ErrorCode = "unavailable"
	ErrorCodeMemoryInvalid              ErrorCode = "memory_invalid"
)
