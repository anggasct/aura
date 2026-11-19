package agent

import "github.com/anggasct/aura/internal/platform/fault"

type ErrorCode string

const (
	ErrorCodeDefinitionInvalid ErrorCode = "agent_definition_invalid"
	ErrorCodeDuplicateID       ErrorCode = "agent_duplicate_id"
	ErrorCodeUnknownCapability ErrorCode = "agent_unknown_capability"
	ErrorCodeUnknownTool       ErrorCode = "agent_unknown_tool"
	ErrorCodeUnknownModelRoute ErrorCode = "agent_unknown_model_route"
	ErrorCodeNotFound          ErrorCode = "agent_not_found"
	ErrorCodeResolutionFailed  ErrorCode = "agent_resolution_failed"
	ErrorCodeBindingInvalid    ErrorCode = "binding_invalid"
	ErrorCodePersonaInvalid    ErrorCode = "persona_invalid"
	ErrorCodePersonaSecret     ErrorCode = "persona_secret_invalid"
)

func codedError(code ErrorCode, detail string) error {
	return fault.New(code, detail)
}
