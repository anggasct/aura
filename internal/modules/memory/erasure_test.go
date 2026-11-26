package memory

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestMemoryEraserAndRekeyerRefuseNilDB(t *testing.T) {
	if _, err := NewEraser(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewEraser(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	if _, err := NewRekeyer(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewRekeyer(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}
