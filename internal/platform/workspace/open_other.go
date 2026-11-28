//go:build !linux

package workspace

import (
	"os"

	"github.com/anggasct/aura/internal/platform/fault"
)

func OpenRead(_, _ string) (*os.File, error) {
	return nil, fault.New(ErrorCodeUnsupported, "race-safe workspace access requires Linux")
}
