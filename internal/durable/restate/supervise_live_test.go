//go:build durable

package restate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitSupervisorRegistered(t *testing.T, supervisor *Supervisor, previousPID int, timeout time.Duration) Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snapshot := supervisor.Snapshot()
		if snapshot.State == SupervisionRunning && snapshot.Registered && snapshot.PID != 0 && snapshot.PID != previousPID {
			return snapshot
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("supervisor never reached a registered running child (previous pid %d): last = %+v", previousPID, supervisor.Snapshot())
	return Snapshot{}
}

func listedDeploymentURIs(t *testing.T, adminURL string) []string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, adminURL+"/deployments", http.NoBody)
	if err != nil {
		t.Fatalf("deployments request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read deployments: %v", err)
	}
	var listed struct {
		Deployments []struct {
			URI string `json:"uri"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &listed); err != nil {
		t.Fatalf("decode deployments %q: %v", body, err)
	}
	uris := make([]string, 0, len(listed.Deployments))
	for _, deployment := range listed.Deployments {
		uris = append(uris, deployment.URI)
	}
	return uris
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func dataDirHolders(dataDir string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var holders []string
	for _, entry := range entries {
		name := entry.Name()
		if name[0] < '0' || name[0] > '9' || name == strconv.Itoa(os.Getpid()) {
			continue
		}
		raw, err := os.ReadFile("/proc/" + name + "/cmdline")
		if err != nil || !bytes.Contains(raw, []byte(dataDir)) {
			continue
		}
		holders = append(holders, name+": "+strings.ReplaceAll(string(bytes.TrimRight(raw, "\x00")), "\x00", " "))
	}
	return holders
}

func requireNoDataDirHolders(t *testing.T, dataDir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		holders := dataDirHolders(dataDir)
		if len(holders) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still hold %s after shutdown: %v", dataDir, holders)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestLiveSupervisorSpawnsRegistersRestartsAndDrains(t *testing.T) {
	binary := liveRestateBinary(t)
	endpoint, _ := startTestEndpoint(t)
	handlerURL := "http://" + endpoint.BoundAddr().String()
	dataDir := filepath.Join(t.TempDir(), "restate-data")
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	supervisor, err := NewSupervisor(&SuperviseConfig{
		BinaryPath:     binary,
		DataDir:        dataDir,
		IngressURL:     fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		AdminURL:       adminURL,
		HandlerURL:     handlerURL,
		ReadyTimeout:   60 * time.Second,
		ShutdownGrace:  10 * time.Second,
		RestartInitial: 100 * time.Millisecond,
		RestartMax:     time.Second,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		killHolders(dataDir)
	})

	first := waitSupervisorRegistered(t, supervisor, 0, 90*time.Second)
	uris := listedDeploymentURIs(t, adminURL)
	registered := false
	for _, uri := range uris {
		if strings.Contains(uri, endpoint.BoundAddr().String()) {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("deployments = %v, want one pointing at %s", uris, handlerURL)
	}

	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill child %d: %v", first.PID, err)
	}
	second := waitSupervisorRegistered(t, supervisor, first.PID, 90*time.Second)
	if second.Restarts < 1 {
		t.Errorf("restarts after child crash = %d, want >= 1", second.Restarts)
	}
	if processAlive(first.PID) {
		t.Errorf("crashed child %d is still alive", first.PID)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("supervisor did not stop within 30s of cancel")
	}
	if state := supervisor.Snapshot().State; state != SupervisionStopped {
		t.Errorf("state after drain = %q, want %q", state, SupervisionStopped)
	}
	if processAlive(second.PID) {
		t.Errorf("child %d survived the drain", second.PID)
	}
	requireNoDataDirHolders(t, dataDir, 15*time.Second)
}
