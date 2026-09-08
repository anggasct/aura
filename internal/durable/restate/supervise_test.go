package restate

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/restate/health" || r.URL.Path == "/version"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/deployments":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(probe.Close)
	return probe
}

func writeSleepStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-restate-server")
	wrapper := "#!/bin/sh\nif [ -n \"$ARGDUMP\" ]; then echo \"$@\" > \"$ARGDUMP\"; fi\nexec sleep 60\n"
	if err := os.WriteFile(path, []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func writeCrashStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crash-restate-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatalf("write crash stub: %v", err)
	}
	return path
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := server.URL
	server.Close()
	return url
}

func TestSupervisorMissingBinaryFailsClosed(t *testing.T) {
	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath: "/nonexistent/restate-server",
		IngressURL: "http://127.0.0.1:8080",
		AdminURL:   "http://127.0.0.1:9070",
		HandlerURL: "http://127.0.0.1:9080",
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	err = supervisor.Start(t.Context())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeBinaryMissing {
		t.Fatalf("Start err = %v, want %s", err, ErrorCodeBinaryMissing)
	}
}

func TestSupervisorMissingBinaryOnPATHFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	supervisor, err := NewSupervisor(&SuperviseConfig{
		IngressURL: "http://127.0.0.1:8080",
		AdminURL:   "http://127.0.0.1:9070",
		HandlerURL: "http://127.0.0.1:9080",
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	err = supervisor.Start(t.Context())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeBinaryMissing {
		t.Fatalf("Start err = %v, want %s", err, ErrorCodeBinaryMissing)
	}
}

func TestSupervisorSupervisedLifecycle(t *testing.T) {
	probe := startProbeServer(t)
	stub := writeSleepStub(t)

	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath:     stub,
		DataDir:        filepath.Join(t.TempDir(), "restate-data"),
		IngressURL:     probe.URL,
		AdminURL:       probe.URL,
		HandlerURL:     "http://127.0.0.1:9080",
		ReadyTimeout:   10 * time.Second,
		RestartInitial: 10 * time.Millisecond,
		RestartMax:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		snapshot := supervisor.Snapshot()
		if snapshot.State == SupervisionRunning && snapshot.Registered && snapshot.PID > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("supervisor never reached running state: %+v", supervisor.Snapshot())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after cancel: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("supervisor did not stop after cancel")
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != SupervisionStopped {
		t.Errorf("final state = %q, want stopped", snapshot.State)
	}
}

func TestSupervisorCrashRestartsWithBoundedBackoff(t *testing.T) {
	stub := writeCrashStub(t)
	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath:     stub,
		DataDir:        filepath.Join(t.TempDir(), "restate-data"),
		IngressURL:     freeLoopback(t),
		AdminURL:       freeLoopback(t),
		HandlerURL:     "http://127.0.0.1:9080",
		ReadyTimeout:   2 * time.Second,
		RestartInitial: 10 * time.Millisecond,
		RestartMax:     50 * time.Millisecond,
		StableReset:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for supervisor.Snapshot().Restarts < 2 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("supervisor restarts = %d, want at least 2: %+v", supervisor.Snapshot().Restarts, supervisor.Snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not stop after cancel")
	}
}

func TestCappedBackoffBounds(t *testing.T) {
	initial, ceiling := time.Second, 30*time.Second
	got := []time.Duration{
		cappedBackoff(initial, ceiling, 1),
		cappedBackoff(initial, ceiling, 2),
		cappedBackoff(initial, ceiling, 3),
		cappedBackoff(initial, ceiling, 6),
		cappedBackoff(initial, ceiling, 20),
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got[i], want[i])
		}
	}
}

func TestCheckExternalFailsClosed(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(healthy.Close)
	if err := CheckExternal(t.Context(), healthy.URL, healthy.URL); err != nil {
		t.Fatalf("CheckExternal healthy: %v", err)
	}
	unreachable := freeLoopback(t)
	if err := CheckExternal(t.Context(), unreachable, unreachable); err == nil {
		t.Fatal("expected unreachable external runtime to fail, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeUnreachable {
		t.Fatalf("CheckExternal err = %v, want %s", err, ErrorCodeUnreachable)
	}
}

func TestResolveServerDataDirDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	dir, err := resolveServerDataDir("")
	if err != nil {
		t.Fatalf("resolveServerDataDir: %v", err)
	}
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "aura", "restate")
	if dir != want {
		t.Errorf("data dir = %q, want %q", dir, want)
	}
	if dir, err := resolveServerDataDir("/custom/data"); err != nil || dir != "/custom/data" {
		t.Errorf("explicit data dir = %q, %v; want /custom/data", dir, err)
	}
}

func TestWriteChildConfigCarriesEndpointPorts(t *testing.T) {
	dataDir := t.TempDir()
	path, err := writeChildConfig(dataDir, "http://127.0.0.1:18080", "http://127.0.0.1:19070")
	if err != nil {
		t.Fatalf("writeChildConfig: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child config: %v", err)
	}
	for _, want := range []string{`bind-address = "127.0.0.1:18080"`, `bind-address = "127.0.0.1:19070"`, "[ingress]", "[admin]"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("child config misses %q:\n%s", want, content)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat child config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("child config mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := writeChildConfig(dataDir, "://bad", "http://127.0.0.1:19070"); err == nil {
		t.Error("expected unparseable ingress URL to fail, got nil")
	}
	if _, err := writeChildConfig(dataDir, "http://127.0.0.1:18080", "http://127.0.0.1"); err == nil {
		t.Error("expected admin URL without port to fail, got nil")
	}
}

func TestSupervisorPassesChildConfigToBinary(t *testing.T) {
	probe := startProbeServer(t)
	stub := writeSleepStub(t)
	argDump := filepath.Join(t.TempDir(), "argv")
	t.Setenv("ARGDUMP", argDump)

	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath:     stub,
		DataDir:        filepath.Join(t.TempDir(), "restate-data"),
		IngressURL:     probe.URL,
		AdminURL:       probe.URL,
		HandlerURL:     "http://127.0.0.1:9080",
		ReadyTimeout:   10 * time.Second,
		RestartInitial: 10 * time.Millisecond,
		RestartMax:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		snapshot := supervisor.Snapshot()
		if snapshot.State == SupervisionRunning && snapshot.Registered {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("supervisor never reached running state: %+v", supervisor.Snapshot())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	argv, err := os.ReadFile(argDump)
	if err != nil {
		t.Fatalf("read argv dump: %v", err)
	}
	fields := strings.Fields(string(argv))
	configFlag := -1
	for i, field := range fields {
		if field == "-c" && i+1 < len(fields) {
			configFlag = i + 1
		}
	}
	if configFlag < 0 {
		t.Fatalf("stub argv misses -c config flag: %q", argv)
	}
	content, err := os.ReadFile(fields[configFlag])
	if err != nil {
		t.Fatalf("read referenced child config: %v", err)
	}
	wantBind := `bind-address = "` + strings.TrimPrefix(probe.URL, "http://") + `"`
	if !strings.Contains(string(content), wantBind) {
		t.Errorf("child config misses stub port binding %q:\n%s", wantBind, content)
	}
}

func startBlackHole(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("black-hole listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

func TestSupervisorBlackHoleReadinessFailsBounded(t *testing.T) {
	blackHole := startBlackHole(t)
	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath:     writeSleepStub(t),
		DataDir:        filepath.Join(t.TempDir(), "restate-data"),
		IngressURL:     blackHole,
		AdminURL:       blackHole,
		HandlerURL:     "http://127.0.0.1:9080",
		ReadyTimeout:   3 * time.Second,
		RestartInitial: time.Hour,
		RestartMax:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	start := time.Now()
	exited := make(chan error)
	procExited, err := supervisor.waitReady(t.Context(), exited)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("waitReady on black-hole ingress succeeded, want unreachable")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeUnreachable {
		t.Fatalf("waitReady err = %v, want %s", err, ErrorCodeUnreachable)
	}
	if procExited {
		t.Errorf("waitReady procExited = true, want false for a hung child")
	}
	if elapsed > 15*time.Second {
		t.Errorf("black-hole readiness took %s, want bounded failure near the 3s ready timeout", elapsed)
	}
	regStart := time.Now()
	regErr := RegisterDeployment(t.Context(), blackHole, "http://127.0.0.1:9080")
	regElapsed := time.Since(regStart)
	if regErr == nil {
		t.Fatal("RegisterDeployment on black-hole admin succeeded, want registration failure")
	}
	if code, ok := CodeOf(regErr); !ok || code != ErrorCodeRegistrationFailed {
		t.Fatalf("RegisterDeployment err = %v, want %s", regErr, ErrorCodeRegistrationFailed)
	}
	if regElapsed > 10*time.Second {
		t.Errorf("black-hole registration took %s, want a bounded per-request timeout", regElapsed)
	}
}
