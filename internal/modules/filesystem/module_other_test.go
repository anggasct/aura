//go:build !linux

package filesystem

import (
	"strings"
	"testing"
)

func TestModuleFailsClosedWithoutLinux(t *testing.T) {
	_, err := New(&Deps{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("New succeeded on a platform without race-safe filesystem access")
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("error = %v, want Linux containment requirement", err)
	}
}
