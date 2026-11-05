//go:build linux && integration

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/sandbox"
)

const (
	integrationRequestTimeout = 3 * time.Second
	integrationHangBound      = integrationRequestTimeout + 6*time.Second
	integrationReapDeadline   = 10 * time.Second
)

func integrationFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/mcp-stdio-integration-helper.sh")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mcp-stdio-integration-helper.sh")
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func processesRunning(t *testing.T, executable string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	var found []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), executable) {
			found = append(found, pid)
		}
	}
	return found
}

func findingsByCode(t *testing.T, started *mcpRuntime) map[string]string {
	t.Helper()
	return findingCodes(started.check.Checker.Check(t.Context()))
}

func TestIntegrationMCPStdioServerInsideTheSandboxSession(t *testing.T) {
	fixture := integrationFixture(t)
	t.Chdir(filepath.Dir(fixture))
	server := mcpTestServer("docs")
	server.Command = fixture
	server.Args = nil
	server.Environment = nil
	server.Capabilities = []string{"fs.read"}
	server.StartupTimeout = config.Duration(30 * time.Second)
	server.RequestTimeout = config.Duration(integrationRequestTimeout)

	harness := newMCPHarness(t, []config.MCPServer{server})
	harness.approveServer(t, "docs")
	started := harness.start(t, newMCPSessionStarter(sandbox.Start))

	names := harness.toolNames()
	for _, want := range []string{"mcp_docs_ping", "mcp_docs_hang"} {
		found := false
		for _, name := range names {
			found = found || name == want
		}
		if !found {
			t.Fatalf("broker tools = %v, want %s discovered from the sandboxed server", names, want)
		}
	}
	if len(processesRunning(t, fixture)) == 0 {
		t.Fatal("no sandboxed server process is running after discovery")
	}
	if err := harness.callTool(t.Context(), "mcp_docs_ping", "ping-1", principal.ClassOwner); err != nil {
		t.Fatalf("call through the sandbox session: %v", err)
	}

	begin := time.Now()
	err := harness.callTool(t.Context(), "mcp_docs_hang", "hang-1", principal.ClassOwner)
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("a call to a server that never answers succeeded")
	}
	if elapsed < integrationRequestTimeout || elapsed > integrationHangBound {
		t.Fatalf("hung call returned after %s, want it bounded by request_timeout (%s)", elapsed, integrationRequestTimeout)
	}
	if got := findingsByCode(t, started); got["docs"] != string(mcp.ErrServerUnavailable) {
		t.Fatalf("findings after the hung call = %v, want %s for docs", got, mcp.ErrServerUnavailable)
	}

	if err := started.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(integrationReapDeadline)
	for len(processesRunning(t, fixture)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("sandboxed server processes %v survived Close", processesRunning(t, fixture))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
