//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunBasicOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	result, err := Run(ctx, &spec, "printf", "hello-sandbox")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}
	if result.Output != "hello-sandbox" {
		t.Fatalf("output = %q", result.Output)
	}
}

// Process-group termination: a child that forks a grandchild and outlives
// the deadline must be killed together with its descendants.
func TestRunKillsWholeProcessGroupOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 300 * time.Millisecond},
	}
	// sh spawns a background grandchild, then sleeps; killing the group
	// must take both down, and Run must report Terminated.
	result, err := Run(ctx, &spec, "sh", "-c", "sleep 60 & sleep 60")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Terminated {
		t.Fatalf("result.Terminated = false, want true (exit=%d)", result.ExitCode)
	}
}

func TestRunCancelKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 30 * time.Second},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := Run(ctx, &spec, "sleep", "60")
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		if !result.Terminated {
			t.Errorf("Terminated = false, want true")
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

// Env allowlist: only the declared variables reach the child, and a secret
// canary from the parent environment never leaks in.
func TestRunEnvAllowlistExcludesParentSecrets(t *testing.T) {
	t.Setenv("AURA_SANDBOX_CANARY", "canary-zz9-6k2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: t.TempDir(),
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	result, err := Run(ctx, &spec, "env")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(result.Output, "AURA_SANDBOX_CANARY") || strings.Contains(result.Output, "canary-zz9-6k2") {
		t.Fatalf("canary leaked into child environment:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "PATH=") {
		t.Fatalf("allowlisted PATH missing from child environment:\n%s", result.Output)
	}
}

// Working-dir confinement: the child sees the declared directory as its
// current directory.
func TestRunWorkingDirConfined(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir: dir,
		AllowEnv:   []string{"PATH=" + os.Getenv("PATH")},
		Limits:     Limits{Timeout: 5 * time.Second},
	}
	result, err := Run(ctx, &spec, "sh", "-c", "test -f marker.txt && pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("marker not visible from working dir (exit=%d, out=%q)", result.ExitCode, result.Output)
	}
	if strings.TrimSpace(result.Output) != dir {
		t.Fatalf("pwd = %q, want %q", result.Output, dir)
	}
}

func TestRunNetworkDeniedByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := Spec{
		WorkingDir:   t.TempDir(),
		AllowEnv:     []string{"PATH=" + os.Getenv("PATH")},
		AllowNetwork: true,
		Limits:       Limits{Timeout: 5 * time.Second},
	}
	_, err := Run(ctx, &spec, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxUnavailable {
		t.Fatalf("Run(AllowNetwork) = %v, want sandbox_unavailable", err)
	}
}

func TestRunInvalidSpecFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Run(ctx, &Spec{Limits: Limits{Timeout: time.Second}}, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Run(empty workdir) = %v, want invalid_argument", err)
	}
	_, err = Run(ctx, &Spec{WorkingDir: t.TempDir()}, "true")
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Run(zero timeout) = %v, want invalid_argument", err)
	}
}

func TestNegotiateReportsPrimitives(t *testing.T) {
	primitives, err := Negotiate()
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !primitives.ProcessGroups {
		t.Fatal("process groups must always be available on Linux")
	}
	t.Logf("primitives: seccomp=%v cgroupv2=%v landlock=%v", primitives.Seccomp, primitives.CgroupV2, primitives.Landlock)
}
