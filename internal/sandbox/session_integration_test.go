//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The session suite needs a runnable child. The fixture is the plain
// interpreter script testdata/session-helper.sh, materialized with the exec
// bit into the test's temporary directory; a host without containment never
// reaches these spawns because Start fails closed with sandbox_unavailable
// and every spawn-dependent test skips itself.
//
// The fixture must stay a plain interpreter script: the confined child runs
// under the seccomp syscall allowlist, and a compiled runtime child (a Go
// test binary, for instance) would issue syscalls that allowlist
// deliberately denies.

func sessionFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session-helper.sh")
	script, err := os.ReadFile("testdata/session-helper.sh")
	if err != nil {
		t.Fatalf("read session fixture: %v", err)
	}
	if err := os.WriteFile(path, script, 0o700); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func sessionRequest(t *testing.T) *SessionRequest {
	t.Helper()
	req := testSessionRequest(t)
	helperPath := sessionFixture(t)
	req.Executable = helperPath
	// The fixture must live under a Landlock-declared root or the confined
	// child could not exec it; the workspace root carries the fixture.
	req.WorkingDir = filepath.Dir(helperPath)
	req.Environment = map[string]string{}
	req.Limits = Limits{Timeout: 5 * time.Second}
	return req
}

func startHelperSession(t *testing.T, req *SessionRequest) *Session {
	t.Helper()
	if req == nil {
		req = sessionRequest(t)
	}
	s, err := Start(t.Context(), req)
	if err != nil {
		t.Skipf("containment unavailable on this host: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	return s
}

func exchange(t *testing.T, s *Session, payload string) string {
	t.Helper()
	response, err := s.Request(t.Context(), []byte(payload))
	if err != nil {
		t.Fatalf("Request(%q): %v", payload, err)
	}
	return string(response)
}

// The child runs under the full mandatory isolation stack and the
// persistent pipes carry exchanges; Start awaits the child's setup result,
// so any isolation-setup failure surfaces here as a typed error with no
// process left behind (proven deterministically by the setup-failure test in
// the integration leg).
func TestIntegrationSessionEchoExchange(t *testing.T) {
	s := startHelperSession(t, nil)
	for i := range 3 {
		want := "hello-" + string(rune('a'+i))
		if got := exchange(t, s, want); got != want {
			t.Fatalf("exchange %d = %q, want %q", i, got, want)
		}
	}
}

// Environment contract: the declared environment reaches the child
// and the parent's ambient environment does not leak in.
func TestIntegrationSessionEnvAllowlist(t *testing.T) {
	t.Setenv("AURA_SESSION_CANARY", "canary-qq7-3k1")
	req := sessionRequest(t)
	req.Environment = map[string]string{"AURA_SESSION_DECLARED": "declared-1w8"}
	s := startHelperSession(t, req)
	if got := exchange(t, s, "@env AURA_SESSION_DECLARED"); got != "declared-1w8" {
		t.Fatalf("declared env = %q, want declared-1w8", got)
	}
	if got := exchange(t, s, "@env AURA_SESSION_CANARY"); got != "" {
		t.Fatalf("canary leaked into session child: %q", got)
	}
}

// A request that exceeds its deadline returns sandbox_timeout and the
// session remains usable for a subsequent valid request.
func TestIntegrationSessionRequestTimeoutThenRecovers(t *testing.T) {
	s := startHelperSession(t, nil)
	if _, err := s.Request(t.Context(), []byte("@slow 2s")); err != nil {
		t.Fatalf("arm slow mode: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	_, err := s.Request(ctx, []byte("late"))
	cancel()
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxTimeout {
		t.Fatalf("slow request = %v, want sandbox_timeout", err)
	}
	if got := exchange(t, s, "after-timeout"); got != "after-timeout" {
		t.Fatalf("session not usable after timeout: %q", got)
	}
}

// A response beyond the output bound returns sandbox_output_exceeded
// and the session remains usable for a subsequent valid request.
func TestIntegrationSessionOutputExceededThenRecovers(t *testing.T) {
	req := sessionRequest(t)
	req.Limits.MaxOutputBytes = 64
	s := startHelperSession(t, req)
	if _, err := s.Request(t.Context(), []byte("@big 4096")); err != nil {
		t.Fatalf("arm big mode: %v", err)
	}
	_, err := s.Request(t.Context(), []byte("big"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSandboxOutputExceeded {
		t.Fatalf("big response = %v, want sandbox_output_exceeded", err)
	}
	if got := exchange(t, s, "after-big"); got != "after-big" {
		t.Fatalf("session not usable after output exceeded: %q", got)
	}
}

// Concurrent Request calls are serialized — every response is the
// echo of its own request, with no interleaved writes on the child's stdin.
// Run with -race: the serialization is what the race detector observes.
func TestIntegrationSessionConcurrentRequestsSerialize(t *testing.T) {
	s := startHelperSession(t, nil)
	const workers = 8
	const each = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				want := "w" + string(rune('0'+w)) + "-" + string(rune('a'+i))
				got, err := s.Request(t.Context(), []byte(want))
				if err != nil {
					errs <- err
					return
				}
				if string(got) != want {
					errs <- errors.New("desynced exchange: sent " + want + " got " + string(got))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Close terminates and reaps the whole group and releases the
// cgroup; a leak harness asserts no process, fd, or cgroup remains, and
// Close is idempotent.
func TestIntegrationSessionCloseReaps(t *testing.T) {
	goroutinesBefore := runtime.NumGoroutine()
	fdsBefore := countOpenFDs(t)
	cgroupsBefore := countAuraCgroups(t)

	req := sessionRequest(t)
	req.Limits.Timeout = 30 * time.Second
	s, err := Start(t.Context(), req)
	if err != nil {
		t.Skipf("containment unavailable on this host: %v", err)
	}
	if _, err := s.Request(t.Context(), []byte("warm")); err != nil {
		t.Fatalf("warm exchange: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	time.Sleep(200 * time.Millisecond)

	if got := runtime.NumGoroutine(); got > goroutinesBefore+2 {
		t.Errorf("goroutine leak: before=%d after=%d", goroutinesBefore, got)
	}
	if got := countOpenFDs(t); got > fdsBefore+2 {
		t.Errorf("fd leak: before=%d after=%d", fdsBefore, got)
	}
	if got := countAuraCgroups(t); got > cgroupsBefore {
		t.Errorf("cgroup leak: before=%d after=%d", cgroupsBefore, got)
	}
	if orphans := countHelperOrphans(t.Context()); orphans != 0 {
		t.Errorf("orphan session-helper processes: %d", orphans)
	}
	_, err = s.Request(t.Context(), []byte("late"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSessionClosed {
		t.Fatalf("request after Close = %v, want session_closed", err)
	}
}

// A closed session refuses further exchanges instead of hanging.
func TestSessionRequestAfterCloseFailsClosed(t *testing.T) {
	s := startHelperSession(t, nil)
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := s.Request(t.Context(), []byte("again"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeSessionClosed {
		t.Fatalf("Request after Close = %v, want session_closed", err)
	}
}

// A child crash surfaces as a stable session error, subsequent
// requests fail closed, and Close still reaps.
func TestIntegrationSessionCrashFailsClosed(t *testing.T) {
	s := startHelperSession(t, nil)
	if _, err := s.Request(t.Context(), []byte("@crash")); err == nil {
		t.Fatal("crash exchange returned nil error, want failure")
	}
	for range 2 {
		_, err := s.Request(t.Context(), []byte("after-crash"))
		if code, ok := CodeOf(err); !ok || code != ErrorCodeSessionBroken {
			t.Fatalf("request after crash = %v, want session_broken", err)
		}
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close after crash: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("second Close after crash = %v, want nil", err)
	}
}

// The session child shares the one-shot stack's
// kernel-level network denial — net namespace without external linkage plus
// the seccomp socket-deny. The property is enforced by the kernel, not by
// child cooperation, so the same-primitive assertion here is that the
// session child exchanges successfully inside that stack; the network
// denial itself is proven for the shared primitives by
// TestIntegrationNetworkDenied in the isolation leg.
func TestIntegrationSessionNoNetworkCapability(t *testing.T) {
	if !usernsAvailable() {
		t.Skip("user namespaces unavailable: session containment (and its net namespace) cannot be provided")
	}
	s := startHelperSession(t, nil)
	if got := exchange(t, s, "confined"); got != "confined" {
		t.Fatalf("exchange = %q", got)
	}
}

// countHelperOrphans counts surviving helper processes after teardown.
func countHelperOrphans(ctx context.Context) int {
	out, err := exec.CommandContext(ctx, "pgrep", "-c", "-f", "session-helper").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}
