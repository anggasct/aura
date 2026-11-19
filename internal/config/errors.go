package config

type ErrorCode string

const (
	ErrorCodeVersionUnsupported ErrorCode = "config_version_unsupported"
	ErrorCodeOwnerSecret        ErrorCode = "owner_secret_invalid"
)
