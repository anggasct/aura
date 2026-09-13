package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func stdioHelperConfig() *config.MCPServer {
	return &config.MCPServer{
		Name:           "shutdown-stdio",
		Transport:      config.MCPTransportStdio,
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestHelperProcess", "--"},
		Environment:    map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		StartupTimeout: config.Duration(10 * time.Second),
		RequestTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func TestStdioCloseReapsProcess(t *testing.T) {
	ctx := t.Context()
	client, err := NewClient(stdioHelperConfig(), nil)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if client.cmd == nil || client.cmd.Process == nil {
		t.Fatal("stdio process not retained")
	}
	pid := client.cmd.Process.Pid
	if err := client.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if client.cmd.ProcessState == nil || !client.cmd.ProcessState.Exited() {
		t.Fatalf("process %d not reaped", pid)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
}

func TestConnectAfterCloseFails(t *testing.T) {
	ctx := t.Context()
	client, err := NewClient(stdioHelperConfig(), nil)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := client.Connect(ctx, nil); err == nil {
		t.Fatal("connect after close accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrServerUnavailable {
		t.Fatalf("code = %v, %v", code, ok)
	}
}

func pollCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", message)
}

func TestConnectCloseCycleLeaksNothing(t *testing.T) {
	ctx := t.Context()
	server := sdk.NewServer(&sdk.Implementation{Name: "cycle-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "ping", Description: "ping"}, func(_ context.Context, _ *sdk.CallToolRequest, _ EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: "pong"}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	serverCfg := &config.MCPServer{
		Name: "cycle", Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL,
		RequestTimeout: config.Duration(10 * time.Second),
		ConnectTimeout: config.Duration(10 * time.Second),
		StartupTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
	baseline := runtime.NumGoroutine()
	for range 3 {
		client, err := NewClient(serverCfg, nil,
			WithHTTPClient(&http.Client{}),
			WithEndpointPolicy(stubEndpointPolicy{}),
		)
		if err != nil {
			t.Fatalf("NewClient(): %v", err)
		}
		if err := client.Connect(ctx, nil); err != nil {
			t.Fatalf("Connect(): %v", err)
		}
		if err := client.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}
	pollCondition(t, 5*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+3
	}, "goroutines did not settle after connect/close cycles")
}

func TestSleeperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "sleeper" {
		return
	}
	lockPath := os.Getenv("MCP_TEST_GRANDCHILD_LOCK_FILE")
	if lockPath == "" {
		os.Exit(2)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(2)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		os.Exit(2)
	}
	time.Sleep(60 * time.Second)
}

func TestHelperProcessWithGrandchild(t *testing.T) {
	mode := os.Getenv("GO_WANT_HELPER_PROCESS")
	if mode != "with-grandchild" && mode != "fail-after-grandchild" {
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestSleeperProcess", "--")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=sleeper")
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	if mode == "fail-after-grandchild" {
		lockPath := os.Getenv("MCP_TEST_GRANDCHILD_LOCK_FILE")
		pollCondition(t, 10*time.Second, func() bool {
			_, err := os.Stat(lockPath)
			return err == nil
		}, "sleeper did not start")
		os.Exit(1)
	}
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "group-helper-server",
		Version: "1.0.0",
	}, nil)
	sdk.AddTool(server, &sdk.Tool{
		Name:        "ping",
		Description: "ping tool",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ EchoInput) (*sdk.CallToolResult, EchoOutput, error) {
		return nil, EchoOutput{Reply: "pong"}, nil
	})
	_ = server.Run(context.Background(), &sdk.StdioTransport{})
	os.Exit(0)
}

func grandchildLockReleased(t *testing.T, lockPath string) bool {
	t.Helper()
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

func groupHelperConfig(mode, lockPath string) *config.MCPServer {
	return &config.MCPServer{
		Name:           "group-shutdown-stdio",
		Transport:      config.MCPTransportStdio,
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestHelperProcessWithGrandchild", "--"},
		Environment:    map[string]string{"GO_WANT_HELPER_PROCESS": mode, "MCP_TEST_GRANDCHILD_LOCK_FILE": lockPath},
		StartupTimeout: config.Duration(10 * time.Second),
		RequestTimeout: config.Duration(10 * time.Second),
		MaxMessageSize: 1 << 20,
	}
}

func TestStdioCloseKillsProcessGroup(t *testing.T) {
	ctx := t.Context()
	lockPath := filepath.Join(t.TempDir(), "grandchild.lock")
	client, err := NewClient(groupHelperConfig("with-grandchild", lockPath), nil)
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	if err := client.Connect(ctx, nil); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if client.cmd == nil || client.cmd.Process == nil {
		t.Fatal("stdio process not retained")
	}
	pollCondition(t, 10*time.Second, func() bool {
		if _, err := os.Stat(lockPath); err != nil {
			return false
		}
		return !grandchildLockReleased(t, lockPath)
	}, "grandchild did not take its lock")
	pid := client.cmd.Process.Pid
	if err := client.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if client.cmd.ProcessState == nil || !client.cmd.ProcessState.Exited() {
		t.Fatalf("process %d not reaped", pid)
	}
	pollCondition(t, 10*time.Second, func() bool {
		return grandchildLockReleased(t, lockPath)
	}, "grandchild survived process group shutdown")
}
