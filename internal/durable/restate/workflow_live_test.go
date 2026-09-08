//go:build durable

package restate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/workflow"
)

type liveServer struct {
	t       *testing.T
	binary  string
	dir     string
	ingress string
	admin   string
	cmd     *exec.Cmd
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func startLiveServer(t *testing.T, binary, dir string, ingressPort, adminPort int) *liveServer {
	t.Helper()
	config := fmt.Sprintf("[ingress]\nbind-address = \"127.0.0.1:%d\"\n\n[admin]\nbind-address = \"127.0.0.1:%d\"\n", ingressPort, adminPort)
	if err := os.WriteFile(filepath.Join(dir, "restate.toml"), []byte(config), 0600); err != nil {
		t.Fatalf("write server config: %v", err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatalf("server data dir: %v", err)
	}
	server := &liveServer{
		t:       t,
		binary:  binary,
		dir:     dir,
		ingress: fmt.Sprintf("http://127.0.0.1:%d", ingressPort),
		admin:   fmt.Sprintf("http://127.0.0.1:%d", adminPort),
	}
	server.start(dataDir)
	return server
}

func killHolders(dataDir string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid := entry.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil {
			continue
		}
		if !bytes.Contains(raw, []byte(dataDir)) {
			continue
		}
		var pidNum int
		if _, err := fmt.Sscanf(pid, "%d", &pidNum); err != nil {
			continue
		}
		if pidNum == os.Getpid() {
			continue
		}
		proc, err := os.FindProcess(pidNum)
		if err != nil {
			continue
		}
		_ = proc.Kill()
	}
}

func (s *liveServer) start(dataDir string) {
	s.t.Helper()
	s.cmd = exec.Command(s.binary, "--base-dir", dataDir, "-c", filepath.Join(s.dir, "restate.toml"), "--bind-port", fmt.Sprint(freePort(s.t)))
	s.cmd.Stdout = os.Stderr
	s.cmd.Stderr = os.Stderr
	if err := s.cmd.Start(); err != nil {
		s.t.Fatalf("start restate-server: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodGet, s.ingress+"/restate/health", http.NoBody)
		if err != nil {
			s.t.Fatalf("health request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			s.stop()
			s.t.Fatal("restate ingress never became ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *liveServer) stop() {
	s.t.Helper()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
		s.cmd = nil
	}
	dataDir := filepath.Join(s.dir, "data")
	killHolders(dataDir)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, s.ingress+"/restate/health", http.NoBody)
		if err != nil {
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return
		}
		_ = response.Body.Close()
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatal("restate-server did not stop")
}

func (s *liveServer) registerDeployment(t *testing.T, handlerAddr string) {
	t.Helper()
	for _, force := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{"uri": "http://" + handlerAddr, "force": force})
		response, err := http.Post(s.admin+"/deployments", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("register deployment: %v", err)
		}
		responseBody, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode < 300 {
			return
		}
		if response.StatusCode != http.StatusConflict || force {
			t.Fatalf("register deployment status = %d: %s", response.StatusCode, responseBody)
		}
	}
}

type countingToolRunner struct {
	mu     sync.Mutex
	called []string
}

func (c *countingToolRunner) Invoke(_ context.Context, toolID string, _ json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.called = append(c.called, toolID)
	return json.RawMessage(`{"ok":true}`), nil
}

func (c *countingToolRunner) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.called...)
}

func liveTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func waitEndpointReady(t *testing.T, endpoint *Endpoint) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if bound := endpoint.BoundAddr(); bound != nil {
			response, err := http.Get("http://" + bound.String() + "/health")
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("handler endpoint never became ready")
}

func waitForStoreStatus(t *testing.T, disk *workflow.Store, runID, status string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		run, err := disk.Run(context.Background(), runID)
		if err == nil && run.Status == status {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s never reached store status %s", runID, status)
}

func waitForAdapterStatus(t *testing.T, adapter *Adapter, ref durable.RunRef, want durable.RunState, within time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		status, err := adapter.Status(ctx, ref)
		if err == nil && status.State == want {
			return
		}
		if err != nil {
			t.Logf("adapter status: %v", err)
		} else {
			t.Logf("adapter status: %s (%s)", status.State, status.Detail)
		}
		time.Sleep(5 * time.Second)
	}
	status, err := adapter.Status(ctx, ref)
	t.Fatalf("run %s never reached adapter status %s: last = %+v, %v", ref.Key, want, status, err)
}

func TestLiveCrashResumeSkipsCompletedSteps(t *testing.T) {
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("restate-server binary is not available: %v", err)
	}
	registry, err := auraagent.Build(nil, []string{"read_file", "list_dir"}, []string{"primary"})
	if err != nil {
		t.Fatalf("agent registry: %v", err)
	}
	ctx := context.Background()
	disk := workflow.NewStore(liveTestDB(t))
	tools := &countingToolRunner{}
	runner := durable.NewFake()
	interpreter := workflow.NewInterpreter(disk, runner, &workflow.Options{
		MaxConcurrentSteps: 1,
		AgentResolver:      registry,
		Tools:              tools,
	})

	readFile, listDir, ciEvent := "read_file", "list_dir", "ci"
	spec := &workflow.Spec{
		ID: "live-proof", Goal: "Live crash resume", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "build", Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &readFile}, Timeout: 30 * time.Second},
			{ID: "hold", DependsOn: []string{"build"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &ciEvent}, Timeout: 5 * time.Minute},
			{ID: "ship", DependsOn: []string{"hold"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &listDir}, Timeout: 30 * time.Second},
		},
	}
	deps := workflow.ValidationDeps{
		KnownTools:     []string{"read_file", "list_dir"},
		EffectfulTools: []string{},
		Agents:         registry,
	}
	if err := interpreter.Load(ctx, spec, deps); err != nil {
		t.Fatalf("Load: %v", err)
	}
	summary, err := disk.CreateRun(ctx, spec, &workflow.RunInput{Objective: "survive restart"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := summary.ID
	durableKey := fmt.Sprintf("live-%d", time.Now().UnixNano())

	dir := t.TempDir()
	ingressPort, adminPort := freePort(t), freePort(t)
	handlerPort := freePort(t)
	handlerAddr := fmt.Sprintf("127.0.0.1:%d", handlerPort)
	server := startLiveServer(t, binary, dir, ingressPort, adminPort)
	t.Cleanup(func() { server.stop() })

	endpoint, err := NewEndpoint(EndpointConfig{HandlerAddr: handlerAddr}, nil)
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	endpoint.RegisterHandler("workflow", func(ctx context.Context, inv durable.Invocation) error {
		_, err := interpreter.DriveSerial(ctx, inv, runID)
		return err
	})
	endpointCtx, stopEndpoint := context.WithCancel(context.Background())
	endpointDone := make(chan error, 1)
	go func() { endpointDone <- endpoint.Start(endpointCtx) }()
	waitEndpointReady(t, endpoint)
	server.registerDeployment(t, handlerAddr)
	t.Cleanup(func() {
		stopEndpoint()
		select {
		case <-endpointDone:
		case <-time.After(10 * time.Second):
		}
	})

	adapter, err := NewAdapter(Config{IngressURL: server.ingress}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	adapter.RegisterHandler("workflow", func(context.Context, durable.Invocation) error { return nil })
	ref, err := adapter.Start(ctx, durable.StartRequest{Handler: "workflow", Key: durableKey, Payload: []byte(`{"objective":"survive restart"}`)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForStoreStatus(t, disk, runID, workflow.RunSuspended, 90*time.Second)
	if got := tools.snapshot(); len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("tool calls before crash = %v, want one read_file", got)
	}

	stopEndpoint()
	<-endpointDone
	server.stop()

	server = startLiveServer(t, binary, dir, ingressPort, adminPort)
	endpoint, err = NewEndpoint(EndpointConfig{HandlerAddr: handlerAddr}, nil)
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	endpoint.RegisterHandler("workflow", func(ctx context.Context, inv durable.Invocation) error {
		_, err := interpreter.DriveSerial(ctx, inv, runID)
		return err
	})
	secondCtx, stopSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- endpoint.Start(secondCtx) }()
	waitEndpointReady(t, endpoint)
	t.Cleanup(func() {
		stopEndpoint()
		stopSecond()
		select {
		case <-endpointDone:
		case <-time.After(10 * time.Second):
		}
		select {
		case <-secondDone:
		case <-time.After(10 * time.Second):
		}
	})
	server.registerDeployment(t, handlerAddr)

	if err := adapter.Signal(ctx, ref, "wait.hold", []byte(`{"ci":"green"}`)); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	waitForAdapterStatus(t, adapter, ref, durable.RunSucceeded, 120*time.Second)
	waitForStoreStatus(t, disk, runID, workflow.RunSucceeded, 30*time.Second)

	if got := tools.snapshot(); len(got) != 2 || got[0] != "read_file" || got[1] != "list_dir" {
		t.Fatalf("tool calls after resume = %v, want build once and ship once", got)
	}
	steps, err := disk.Steps(ctx, runID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	for _, step := range steps {
		if step.Status != workflow.StepSucceeded {
			t.Errorf("step %s = %s, want %s", step.StepID, step.Status, workflow.StepSucceeded)
		}
	}
}
