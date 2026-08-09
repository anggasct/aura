//go:build linux && integration

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise the fully confined Run path: the re-exec child runs in
// fresh user and network namespaces with Landlock, seccomp, rlimit, and cgroup
// enforcement. They require a kernel and runtime that allow an unprivileged
// binary to create user namespaces and to write cgroup controllers — run with
// `go test -tags=integration` in such an environment (e.g. as root, which
// bypasses AppArmor's unprivileged-userns restriction).

// TestMain intercepts the sandbox-child sentinel so the re-executed test
// binary runs RunChild instead of the test suite. The production aura binary
// dispatches the same sentinel from cmd/aura/main.go.
func TestMain(m *testing.M) {
	if IsChild(os.Args) {
		os.Exit(RunChild())
	}
	os.Exit(m.Run())
}

func baseSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 10 * time.Second},
	}
}

func TestIntegrationRunBasicOutput(t *testing.T) {
	spec := baseSpec(t)
	result, err := Run(context.Background(), &spec, "printf", "hello-sandbox")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 || result.Output != "hello-sandbox" {
		t.Fatalf("result=%+v, want exit 0 output hello-sandbox", result)
	}
}

func TestIntegrationEnvAllowlistExcludesParentSecrets(t *testing.T) {
	t.Setenv("AURA_SANDBOX_CANARY", "canary-zz9-6k2")
	spec := baseSpec(t)
	result, err := Run(context.Background(), &spec, "env")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Output, "AURA_SANDBOX_CANARY") || strings.Contains(result.Output, "canary-zz9-6k2") {
		t.Fatalf("canary leaked into child:\n%s", result.Output)
	}
}

func TestIntegrationWorkingDirConfined(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec := baseSpec(t)
	spec.WorkingDir = dir
	result, err := Run(context.Background(), &spec, "sh", "-c", "test -f marker.txt && pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d out=%q", result.ExitCode, result.Output)
	}
	if strings.TrimSpace(result.Output) != dir {
		t.Fatalf("pwd=%q want %q", result.Output, dir)
	}
}

func TestIntegrationKillsWholeProcessGroupOnTimeout(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.Timeout = 400 * time.Millisecond
	result, err := Run(context.Background(), &spec, "sh", "-c", "sleep 60 & sleep 60")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Terminated {
		t.Fatalf("Terminated=false (exit=%d)", result.ExitCode)
	}
}

func TestIntegrationCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spec := baseSpec(t)
	spec.Limits.Timeout = 30 * time.Second
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := Run(ctx, &spec, "sleep", "60")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		if !result.Terminated {
			t.Errorf("Terminated=false")
		}
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestIntegrationOutputCapped(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.MaxOutputBytes = 16
	result, err := Run(context.Background(), &spec, "sh", "-c", "printf '0123456789abcdefGHIJKLMNOP'")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Truncated || len(result.Output) != 16 {
		t.Fatalf("Truncated=%v len=%d", result.Truncated, len(result.Output))
	}
}

// Landlock denies a read outside the declared roots. The workspace is
// declared read-write; a sibling directory outside it is not, so a tool that
// reads a file there must fail.
func TestIntegrationLandlockDeniesEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("treasure"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec := baseSpec(t)
	result, _ := Run(context.Background(), &spec, "cat", secret)
	if strings.Contains(result.Output, "treasure") {
		t.Fatalf("Landlock escape: child read outside-declared file (%+v)", result)
	}
	if result.ExitCode == 0 {
		t.Fatalf("Landlock escape: cat succeeded outside declared roots")
	}
}

// An rlimit file-size bound kills a tool that writes past it. The writing
// command is the whole script so its signal exit is the run's exit.
func TestIntegrationRlimitFileBytes(t *testing.T) {
	spec := baseSpec(t)
	spec.Limits.FileBytes = 16
	result, err := Run(context.Background(), &spec, "sh", "-c", "yes | head -c 4096 > big")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("rlimit FSIZE not enforced: child wrote past the bound (%+v)", result)
	}
}
