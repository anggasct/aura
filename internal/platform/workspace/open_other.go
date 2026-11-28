//go:build !linux

package workspace

import (
	"os"
)

func OpenRead(_, _ string) (*os.File, error) {
	return nil, ErrUnsupported()
}
