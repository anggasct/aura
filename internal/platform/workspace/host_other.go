//go:build !unix

package workspace

import (
	"os"

	"github.com/anggasct/aura/internal/platform/fault"
)

func OpenHost(string) (*os.File, error) {
	return nil, fault.New(ErrorCodeUnsupported, "host files require a Unix platform")
}
