package workspace

import (
	"runtime"

	"github.com/anggasct/aura/internal/platform/fault"
)

// Supported reports whether this platform provides race-safe workspace file access.
func Supported() bool {
	return runtime.GOOS == "linux"
}

// ErrUnsupported reports that this platform cannot open workspace files at all.
func ErrUnsupported() error {
	return fault.New(ErrorCodeUnsupported, "race-safe workspace access requires Linux")
}
