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
	listener, err := buildProbeListener(&cfg)
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
	readyBody := probeBody(t, addr, "/readyz")
	// Sandbox availability depends on the host the suite runs on; degraded
	// sandbox still admits intake, so both ready codes are correct here.
	if readyBody.Status != health.ProbeStatusReady ||
		(readyBody.Code != health.ProbeCodeReady && readyBody.Code != health.ProbeCodeReadyDegraded) {
		t.Errorf("readyz body = %+v", readyBody)
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
	if _, err := buildProbeListener(nil); err == nil {
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

func mkdirBackup(t *testing.T, dataRoot string) error {
	t.Helper()
	return os.MkdirAll(filepath.Join(dataRoot, "backups", strconv.FormatInt(time.Now().Unix(), 10)), 0o700)
}
