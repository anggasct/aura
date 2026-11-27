package configkit

import (
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestValidateListenAddress(t *testing.T) {
	t.Parallel()

	valid := []string{
		"127.0.0.1:8282",
		"[::1]:1",
		"localhost:65535",
		"0.0.0.0:443",
	}
	for _, address := range valid {
		t.Run("valid/"+address, func(t *testing.T) {
			t.Parallel()
			if err := ValidateListenAddress("listen_address", address); err != nil {
				t.Fatalf("ValidateListenAddress(%q) = %v, want nil", address, err)
			}
		})
	}

	invalid := []string{
		"",
		"no-port",
		":8282",
		"127.0.0.1:0",
		"127.0.0.1:-1",
		"127.0.0.1:65536",
		"127.0.0.1:abc",
	}
	for _, address := range invalid {
		t.Run("invalid/"+address, func(t *testing.T) {
			t.Parallel()
			err := ValidateListenAddress("listen_address", address)
			if err == nil {
				t.Fatalf("ValidateListenAddress(%q) = nil, want error", address)
			}
			if !fault.HasCode(err, ErrorCodeInvalid) {
				t.Fatalf("ValidateListenAddress(%q) error = %v, want %s", address, err, ErrorCodeInvalid)
			}
			fieldErr, ok := errors.AsType[*FieldError](err)
			if !ok || fieldErr.Path != "listen_address" {
				t.Fatalf("FieldError = %+v (ok=%v), want path listen_address", fieldErr, ok)
			}
		})
	}
}
