package config

type ErrorCode string

const (
	ErrorCodeVersionUnsupported   ErrorCode = "config_version_unsupported"
	ErrorCodeOwnerSecret          ErrorCode = "owner_secret_invalid"
	ErrorCodeOwnerIdentityInvalid ErrorCode = "owner_identity_invalid"
	ErrorCodePersonaInvalid       ErrorCode = "persona_invalid"
	ErrorCodePersonaSecret        ErrorCode = "persona_secret_invalid"
	ErrorCodeBindingInvalid       ErrorCode = "binding_invalid"
)
