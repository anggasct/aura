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

func TestIntegrationSessionEchoExchange(t *testing.T) {
	s := startHelperSession(t, nil)
	for i := range 3 {
		want := "hello-" + string(rune('a'+i))
		if got := exchange(t, s, want); got != want {
			t.Fatalf("exchange %d = %q, want %q", i, got, want)
		}
	}
}

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

func TestIntegrationSessionCtxCancelTearsDown(t *testing.T) {
	goroutinesBefore := runtime.NumGoroutine()
	fdsBefore := countOpenFDs(t)
	cgroupsBefore := countAuraCgroups(t)

	ctx, cancel := context.WithCancel(t.Context())
	req := sessionRequest(t)
	req.Limits.Timeout = 30 * time.Second
	s, err := Start(ctx, req)
	if err != nil {
		cancel()
		t.Skipf("containment unavailable on this host: %v", err)
	}
	if _, err := s.Request(ctx, []byte("warm")); err != nil {
		cancel()
		t.Fatalf("warm exchange: %v", err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := s.Request(t.Context(), []byte("late")); err == nil {
			if time.Now().After(deadline) {
				t.Fatal("session still serving exchanges 5s after Start-context cancel")
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		break
	}
	if _, err := s.Request(t.Context(), []byte("late")); err == nil {
		t.Fatal("request after ctx-cancel succeeded, want failure")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSessionClosed {
		t.Fatalf("request after ctx-cancel = %v, want session_closed", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close after ctx-cancel: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("second Close after ctx-cancel = %v, want nil", err)
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
}

func TestIntegrationSessionNoNetworkCapability(t *testing.T) {
	if !usernsAvailable() {
		t.Skip("user namespaces unavailable: session containment (and its net namespace) cannot be provided")
	}
	s := startHelperSession(t, nil)
	if got := exchange(t, s, "confined"); got != "confined" {
		t.Fatalf("exchange = %q", got)
	}
}

func TestIntegrationSessionConnectDenied(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available for the network-attempt fixture")
	}
	if !usernsAvailable() {
		t.Skip("user namespaces unavailable: session containment (and its seccomp filter) cannot be provided")
	}
	s := startHelperSession(t, nil)
	if got := exchange(t, s, "warm"); got != "warm" {
		t.Fatalf("warm exchange = %q, want echo", got)
	}
	got := exchange(t, s, "@connect")
	if got != "denied-159" {
		t.Fatalf("connect probe = %q, want denied-159 (SIGSYS kill of the connector)", got)
	}
	if got := exchange(t, s, "after-connect"); got != "after-connect" {
		t.Fatalf("session not usable after connect denial: %q", got)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

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
