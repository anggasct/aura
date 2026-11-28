package workspace

type ErrorCode string

const (
	ErrorCodeOutside     ErrorCode = "workspace_outside"
	ErrorCodeUnsupported ErrorCode = "workspace_unsupported"
	ErrorCodeSymlink     ErrorCode = "workspace_symlink"
)
