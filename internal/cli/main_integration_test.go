//go:build linux && integration

package cli

import (
	"os"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
)

func TestMain(m *testing.M) {
	if sandbox.IsChild(os.Args) {
		os.Exit(sandbox.RunChild())
	}
	os.Exit(m.Run())
}
