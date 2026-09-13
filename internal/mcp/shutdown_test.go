package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
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
