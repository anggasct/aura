package workspace

import (
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	DetailNotRelative = "path must be relative to the workspace"
	DetailTraversal   = "path traversal is not allowed"
)

func ValidateRelative(path string) error {
	if path == "" || filepath.IsAbs(path) {
		return fault.New(ErrorCodeOutside, DetailNotRelative)
	}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fault.New(ErrorCodeOutside, DetailTraversal)
		}
	}
	if filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
		return fault.New(ErrorCodeOutside, DetailTraversal)
	}
	return nil
}
