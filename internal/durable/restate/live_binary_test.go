//go:build durable

package restate

import (
	"os"
	"testing"
)

func liveRestateBinary(t *testing.T) string {
	t.Helper()
	required := os.Getenv("AURA_DURABLE_REQUIRE_BINARY") == "1"
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		if required {
			t.Fatal("AURA_RESTATE_BINARY is not set and AURA_DURABLE_REQUIRE_BINARY=1")
		}
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		if required {
			t.Fatalf("restate-server binary is not available and AURA_DURABLE_REQUIRE_BINARY=1: %v", err)
		}
		t.Skipf("restate-server binary is not available: %v", err)
	}
	return binary
}
