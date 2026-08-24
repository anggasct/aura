package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/store"
)

// The probe listener must serve the documented minimal bodies: liveness
// independent of subsystem state, readiness reflecting the evaluation, both
// only via GET/HEAD.
func TestProbeListenerServesLivezAndReadyz(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := store.OpenDB(t.Context(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := mkdirBackup(t, dataRoot); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	listener, err := buildProbeListener(&cfg, nil)
	if err != nil {
		t.Fatalf("buildProbeListener: %v", err)
	}

	// Bind an ephemeral loopback port for the test.
	bind, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	addr := bind.Addr().String()
	_ = bind.Close()
	listener.listen = addr

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listener.Start(ctx) }()
	waitForProbe(t, addr, "/livez")

	liveBody := probeBody(t, addr, "/livez")
	if liveBody.Status != health.ProbeStatusAlive {
		t.Errorf("livez status = %q", liveBody.Status)
	}
	// Readiness reflects the real host: a host missing mandatory sandbox
	// primitives must be 503 with the sandbox code, a fully contained host
	// must be 200 ready. Everything in between is an intake-blocking bug.
	primitives, _ := sandbox.Negotiate()
	supported := len(sandbox.MissingMandatory(primitives)) == 0
	readyCode, readyStatus := rawProbeStatus(t, addr, "/readyz")
	if supported {
		if readyStatus != http.StatusOK || readyCode != health.ProbeStatusReady {
			t.Errorf("readyz on capable host = %d %q", readyStatus, readyCode)
		}
	} else {
		if readyStatus != http.StatusServiceUnavailable ||
			readyCode != "sandbox.sandbox_unavailable" {
			t.Errorf("readyz on host without sandbox = %d %q, want 503 sandbox.sandbox_unavailable", readyStatus, readyCode)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe listener did not stop on cancellation")
	}
}

func TestProbeListenerRejectsNilConfig(t *testing.T) {
	if _, err := buildProbeListener(nil, nil); err == nil {
		t.Fatal("nil config must be rejected")
	}
}

func waitForProbe(t *testing.T, addr, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := probeGet(t.Context(), addr, path)
		if err == nil {
			_ = response.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("probe %s never became reachable", path)
}

func probeGet(ctx context.Context, addr, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func probeBody(t *testing.T, addr, path string) health.ProbeBody {
	t.Helper()
	response, err := probeGet(t.Context(), addr, path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, response.StatusCode)
	}
	var body health.ProbeBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s body: %v", path, err)
	}
	return body
}

// rawProbeStatus returns the probe code and HTTP status so tests can assert
// the readiness 503 matrix against the real host state.
func rawProbeStatus(t *testing.T, addr, path string) (code string, status int) {
	t.Helper()
	response, err := probeGet(t.Context(), addr, path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	var body health.ProbeBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s body: %v", path, err)
	}
	return body.Code, response.StatusCode
}

func mkdirBackup(t *testing.T, dataRoot string) error {
	t.Helper()
	return os.MkdirAll(filepath.Join(dataRoot, "backups", strconv.FormatInt(time.Now().Unix(), 10)), 0o700)
}
