package profile

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestProfileRekeyerRefusesNilDB(t *testing.T) {
	if _, err := NewRekeyer(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewRekeyer(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}
