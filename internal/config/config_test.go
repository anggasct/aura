package config

import (
	"testing"
)

func TestValidateBaseURLLoopbackRange(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.2:8080", "http://127.255.255.254", "http://[::1]:8080", "http://localhost."} {
		if err := ValidateBaseURL(raw); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want nil", raw, err)
		}
	}
	if err := ValidateBaseURL("http://example.com"); err == nil {
		t.Error("ValidateBaseURL(http://example.com) accepted a non-loopback http URL")
	}
}
