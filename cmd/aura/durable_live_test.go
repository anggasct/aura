//go:build durable

package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func liveRestateBinary(t *testing.T) string {
	t.Helper()
	required := os.Getenv("AURA_DURABLE_REQUIRE_BINARY") == "1"
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		if required {
			t.Fatal("AURA_RESTATE_BINARY is not set and AURA_DURABLE_REQUIRE_BINARY=1")
		}
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		if required {
			t.Fatalf("restate-server binary is not available and AURA_DURABLE_REQUIRE_BINARY=1: %v", err)
		}
		t.Skipf("restate-server binary is not available: %v", err)
	}
	return binary
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}

func buildAuraBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "aura")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", out, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return out
}

func dataDirHolders(dataDir string) ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
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
	return holders, nil
}

func killDataDirHolders(dataDir string) {
	holders, err := dataDirHolders(dataDir)
	if err != nil {
		return
	}
	for _, holder := range holders {
		var pid int
		if _, err := fmt.Sscanf(holder, "%d:", &pid); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func writeSupervisedConfig(t *testing.T, root, restateBinary string) (configPath, restateDataDir string) {
	t.Helper()
	restateDataDir = filepath.Join(root, "restate")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	config := fmt.Sprintf(`version: 1
storage:
  path: %s
health:
  listen: %s
models:
  providers:
    loopback:
      protocol: openai_chat_compat
      base_url: http://%s/v1
      auth_scheme: bearer
      session_behavior: none
  definitions:
    primary:
      provider: loopback
      model: smoke
      capabilities:
        streaming: true
        tools: true
        context_tokens: 8192
        tokenizer: cl100k
tools:
  workspace: %s
durable:
  enabled: true
  mode: supervised
  binary_path: %s
  data_dir: %s
  endpoint: http://%s
  admin_endpoint: http://%s
  handler_addr: %s
  node_addr: %s
`, filepath.Join(root, "data"), freeLoopbackAddr(t), freeLoopbackAddr(t), workspace, restateBinary, restateDataDir,
		freeLoopbackAddr(t), freeLoopbackAddr(t), freeLoopbackAddr(t), freeLoopbackAddr(t))
	configPath = filepath.Join(root, "aura.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath, restateDataDir
}

func TestServerSupervisedDurableRuntimeRegistersAndDrains(t *testing.T) {
	restateBinary := liveRestateBinary(t)
	aura := buildAuraBinary(t)
	root := t.TempDir()
	configPath, restateDataDir := writeSupervisedConfig(t, root, restateBinary)
	env := append(os.Environ(),
		"HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "xdg-config"),
		"XDG_DATA_HOME="+filepath.Join(root, "xdg-data"),
		"XDG_STATE_HOME="+filepath.Join(root, "xdg-state"),
		"XDG_CACHE_HOME="+filepath.Join(root, "xdg-cache"),
	)
	t.Cleanup(func() { killDataDirHolders(restateDataDir) })

	serverLog := &lockedBuffer{}
	server := exec.CommandContext(t.Context(), aura, "server", "--config", configPath)
	server.Env = env
	server.Stdout = serverLog
	server.Stderr = serverLog
	if err := server.Start(); err != nil {
		t.Fatalf("start aura server: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = server.Process.Kill()
			<-exited
		}
	})

	var status []byte
	deadline := time.Now().Add(90 * time.Second)
	for {
		select {
		case err := <-exited:
			reaped = true
			t.Fatalf("aura server exited before registration: %v\n%s", err, serverLog.String())
		default:
		}
		probe := exec.CommandContext(t.Context(), aura, "durable", "status", "--config", configPath)
		probe.Env = env
		status, _ = probe.CombinedOutput()
		if bytes.Contains(status, []byte("deployment: registered")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable status never reported a registered deployment:\n%s\nserver log:\n%s", status, serverLog.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, want := range []string{"mode: supervised", "ingress: reachable", "admin: reachable"} {
		if !bytes.Contains(status, []byte(want)) {
			t.Errorf("durable status missing %q:\n%s", want, status)
		}
	}
	if holders, err := dataDirHolders(restateDataDir); err != nil {
		t.Logf("process holder inspection unavailable: %v", err)
	} else if len(holders) == 0 {
		t.Fatalf("no restate-server child holds %s while registered", restateDataDir)
	}

	if err := server.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal aura server: %v", err)
	}
	select {
	case err := <-exited:
		reaped = true
		if err != nil {
			t.Fatalf("aura server exit after SIGTERM = %v, want 0\n%s", err, serverLog.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("aura server did not exit within 30s of SIGTERM\n%s", serverLog.String())
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		holders, err := dataDirHolders(restateDataDir)
		if err != nil {
			t.Logf("process holder inspection unavailable after shutdown: %v", err)
			break
		}
		if len(holders) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restate-server outlived aura server: %v", holders)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
