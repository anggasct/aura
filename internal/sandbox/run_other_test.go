//go:build !linux

package sandbox

import (
	"context"
	"os"
	"testing"
	"time"
)

// On non-Linux every run fails closed before a child exists, and the
// capability layer reports effectful capabilities unavailable.
func TestNonLinuxFailsClosed(t *testing.T) {
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	_, err := Run(context.Background(), &spec, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("Run = %v, want sandbox_unavailable on non-Linux", err)
	}
	if _, err := Negotiate(); err == nil {
		t.Fatal("Negotiate must fail closed on non-Linux")
	}
}
