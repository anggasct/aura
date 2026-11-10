package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/mcp"
)

const (
	mcpSecretArg = "sekrit-arg-value"
	mcpSecretEnv = "sekrit-env-value"
	mcpRemoteURL = "https://mcp.example.com/private/path"
)

func mcpServersYAML(args string) string {
	return `mcp:
  servers:
    - name: docs
      transport: stdio
      command: /usr/local/bin/docs-mcp
      args: ["` + args + `"]
      environment:
        API_KEY: ` + mcpSecretEnv + `
      capabilities: ["fs.read"]
    - name: remote
      transport: streamable_http
      url: ` + mcpRemoteURL + `
`
}

func writeMCPConfig(t *testing.T, dataRoot, args, extra string) string {
	t.Helper()
	return writeAgentsConfig(t, "storage:\n  path: "+dataRoot+"\n"+mcpServersYAML(args)+extra)
}

type mcpListJSON struct {
	Servers []struct {
		Name         string   `json:"name"`
		Transport    string   `json:"transport"`
		State        string   `json:"state"`
		DigestPrefix string   `json:"digest_prefix"`
		Tools        []string `json:"tools"`
	} `json:"servers"`
}

func listMCPJSON(t *testing.T, cfg string) mcpListJSON {
	t.Helper()
	out, err := execute(t, "mcp", "list", "--json", "--config", cfg)
	if err != nil {
		t.Fatalf("mcp list --json: %v", err)
	}
	var decoded mcpListJSON
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return decoded
}

func TestMCPListShowsStateWithoutSecrets(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--token="+mcpSecretArg, "")

	text, err := execute(t, "mcp", "list", "--config", cfg)
	if err != nil {
		t.Fatalf("mcp list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 3 || lines[0] != "SERVER\tTRANSPORT\tSTATE\tDIGEST\tTOOLS" {
		t.Fatalf("list output = %q", text)
	}
	for _, want := range []string{"docs\tstdio\ttrust_required\t", "remote\tstreamable_http\ttrust_required\t", "\t-\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("list output lacks %q:\n%s", want, text)
		}
	}
	jsonOut, err := execute(t, "mcp", "list", "--json", "--config", cfg)
	if err != nil {
		t.Fatalf("mcp list --json: %v", err)
	}
	for _, out := range []string{text, jsonOut} {
		for _, forbidden := range []string{mcpSecretArg, mcpSecretEnv, "mcp.example.com", "private/path", "https://", "/usr/local/bin", "API_KEY", "fs.read"} {
			if strings.Contains(out, forbidden) {
				t.Errorf("output leaks %q:\n%s", forbidden, out)
			}
		}
	}
	decoded := listMCPJSON(t, cfg)
	if len(decoded.Servers) != 2 {
		t.Fatalf("servers = %+v", decoded.Servers)
	}
	for _, server := range decoded.Servers {
		if server.State != "trust_required" || len(server.DigestPrefix) != 12 || server.Tools == nil || len(server.Tools) != 0 {
			t.Errorf("server = %+v", server)
		}
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "mcp", "trust.json")); !os.IsNotExist(err) {
		t.Errorf("listing wrote trust state: %v", err)
	}
}

func TestMCPListWithoutServers(t *testing.T) {
	cfg := writeAgentsConfig(t, "storage:\n  path: "+t.TempDir()+"\n")
	text, err := execute(t, "mcp", "list", "--config", cfg)
	if err != nil || strings.TrimSpace(text) != "no mcp servers configured" {
		t.Fatalf("list = %q, %v", text, err)
	}
	if decoded := listMCPJSON(t, cfg); decoded.Servers == nil || len(decoded.Servers) != 0 {
		t.Fatalf("json servers = %#v, want an empty list", decoded.Servers)
	}
}

func TestMCPTrustApproveBindsToCurrentDigest(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--token="+mcpSecretArg, "")

	out, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.HasPrefix(out, "approved docs (stdio, digest ") || !strings.Contains(out, "takes effect at the next server start") {
		t.Errorf("approve output = %q", out)
	}
	for _, forbidden := range []string{mcpSecretArg, mcpSecretEnv, "/usr/local/bin"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("approve output leaks %q", forbidden)
		}
	}
	states := map[string]string{}
	for _, server := range listMCPJSON(t, cfg).Servers {
		states[server.Name] = server.State
	}
	if states["docs"] != "approved" || states["remote"] != "trust_required" {
		t.Fatalf("states after approve = %v", states)
	}

	again, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg)
	if err != nil || !strings.Contains(again, "docs is already approved") || !strings.Contains(again, "no change") {
		t.Fatalf("repeat approve = %q, %v", again, err)
	}

	edited := writeMCPConfig(t, dataRoot, "--token=other", "")
	states = map[string]string{}
	for _, server := range listMCPJSON(t, edited).Servers {
		states[server.Name] = server.State
	}
	if states["docs"] != "digest_changed" {
		t.Fatalf("state after edit = %v", states)
	}
	if _, err := execute(t, "mcp", "trust", "approve", "docs", "--config", edited); err != nil {
		t.Fatalf("approve after edit: %v", err)
	}
	if got := listMCPJSON(t, edited).Servers[0].State; got != "approved" {
		t.Fatalf("state after re-approval = %q", got)
	}
}

func TestMCPTrustFileIsOwnerOnlyAndAtomic(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--x", "")
	for _, name := range []string{"docs", "remote"} {
		if _, err := execute(t, "mcp", "trust", "approve", name, "--config", cfg); err != nil {
			t.Fatalf("approve %s: %v", name, err)
		}
	}
	dir := filepath.Join(dataRoot, "mcp")
	info, err := os.Stat(filepath.Join(dir, "trust.json"))
	if err != nil {
		t.Fatalf("trust file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("trust file mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("trust dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("trust dir mode = %o, want 700", dirInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("trust dir entries = %v, %v, want only trust.json", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "trust.json"))
	if err != nil {
		t.Fatalf("read trust file: %v", err)
	}
	for _, forbidden := range []string{mcpSecretEnv, mcpRemoteURL, "/usr/local/bin/docs-mcp", "API_KEY"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("trust file stores %q", forbidden)
		}
	}
}

func TestMCPTrustApprovesChangedToolSurface(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--x", "")
	if _, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg); err != nil {
		t.Fatalf("approve: %v", err)
	}
	registry, err := mcp.NewFileTrustRegistry(filepath.Join(dataRoot, "mcp", "trust.json"))
	if err != nil {
		t.Fatalf("NewFileTrustRegistry: %v", err)
	}
	if err := registry.SaveSessionTrust(t.Context(), "docs", "surface-2", nil, []string{"write", "read"}); err != nil {
		t.Fatalf("SaveSessionTrust: %v", err)
	}
	if got := listMCPJSON(t, cfg).Servers[0].State; got != "digest_changed" {
		t.Fatalf("state with a pending tool surface = %q", got)
	}
	out, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg)
	if err != nil || !strings.Contains(out, "approved the changed tool surface of docs (2 tools: read, write") {
		t.Fatalf("approve = %q, %v", out, err)
	}
	if got := listMCPJSON(t, cfg).Servers[0].State; got != "approved" {
		t.Fatalf("state after tool surface approval = %q", got)
	}
}

func TestMCPListShowsOnlyThePinnedToolSurface(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--x", "")
	if _, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg); err != nil {
		t.Fatalf("approve: %v", err)
	}
	registry, err := mcp.NewFileTrustRegistry(filepath.Join(dataRoot, "mcp", "trust.json"))
	if err != nil {
		t.Fatalf("NewFileTrustRegistry: %v", err)
	}
	if err := registry.SaveSessionTrust(t.Context(), "docs", "surface-1", nil, []string{"write", "read"}); err != nil {
		t.Fatalf("SaveSessionTrust: %v", err)
	}
	if got := listMCPJSON(t, cfg).Servers[0]; len(got.Tools) != 0 {
		t.Fatalf("tools of an unpinned surface = %v, want none", got.Tools)
	}
	if err := registry.Approve(t.Context(), "docs", "surface-1"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	decoded := listMCPJSON(t, cfg)
	if got := decoded.Servers[0].Tools; !slices.Equal(got, []string{"read", "write"}) {
		t.Fatalf("pinned tools = %v, want [read write]", got)
	}
	if got := decoded.Servers[1].Tools; got == nil || len(got) != 0 {
		t.Fatalf("tools of a server without a record = %#v, want an empty list", got)
	}
	text, err := execute(t, "mcp", "list", "--config", cfg)
	if err != nil {
		t.Fatalf("mcp list: %v", err)
	}
	if !regexp.MustCompile(`(?m)^docs\tstdio\tapproved\t[0-9a-f]{12}\tread,write$`).MatchString(text) || !regexp.MustCompile(`(?m)^remote\tstreamable_http\ttrust_required\t[0-9a-f]{12}\t-$`).MatchString(text) {
		t.Fatalf("list output = %q", text)
	}
	if err := registry.SaveSessionTrust(t.Context(), "docs", "surface-2", nil, []string{"read", "write", "delete"}); err != nil {
		t.Fatalf("SaveSessionTrust drift: %v", err)
	}
	if got := listMCPJSON(t, cfg).Servers[0]; got.State != "digest_changed" || len(got.Tools) != 0 {
		t.Fatalf("drifted server = %+v, want digest_changed with no pinned tools", got)
	}
}

func TestMCPTrustRevoke(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--x", "")
	if _, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg); err != nil {
		t.Fatalf("approve: %v", err)
	}
	out, err := execute(t, "mcp", "trust", "revoke", "docs", "--config", cfg)
	if err != nil || out != "revoked trust for docs; takes effect at the next server start\n" {
		t.Fatalf("revoke = %q, %v", out, err)
	}
	if got := listMCPJSON(t, cfg).Servers[0].State; got != "trust_required" {
		t.Fatalf("state after revoke = %q", got)
	}
	out, err = execute(t, "mcp", "trust", "revoke", "docs", "--config", cfg)
	if err != nil || out != "docs had no trust record; no change\n" {
		t.Fatalf("second revoke = %q, %v", out, err)
	}
}

func TestMCPTrustRevokeCleansRecordsOfRemovedServers(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--x", "")
	if _, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg); err != nil {
		t.Fatalf("approve: %v", err)
	}
	without := writeAgentsConfig(t, "storage:\n  path: "+dataRoot+"\n")
	if out, err := execute(t, "mcp", "trust", "revoke", "docs", "--config", without); err != nil || !strings.Contains(out, "revoked trust for docs") {
		t.Fatalf("revoke of a removed server = %q, %v", out, err)
	}
}

func TestMCPTrustUnknownServersFailWithStableCode(t *testing.T) {
	cfg := writeMCPConfig(t, t.TempDir(), "--x", "")
	for _, args := range [][]string{
		{"mcp", "trust", "approve", "ghost", "--config", cfg},
		{"mcp", "trust", "revoke", "ghost", "--config", cfg},
	} {
		_, err := execute(t, args...)
		if code, ok := mcp.CodeOf(err); !ok || code != mcp.ErrConfigInvalid {
			t.Errorf("%v error = %v (code %q, %v), want mcp_config_invalid", args[2:4], err, code, ok)
		}
	}
	if _, err := execute(t, "mcp", "trust", "approve", "--config", cfg); err == nil {
		t.Error("approve without a server name accepted")
	}
}

func TestMCPListFailsOnCorruptTrustFile(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := writeMCPConfig(t, dataRoot, "--x", "")
	if err := os.MkdirAll(filepath.Join(dataRoot, "mcp"), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{broken")
	path := filepath.Join(dataRoot, "mcp", "trust.json")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, "mcp", "list", "--config", cfg); err == nil {
		t.Fatal("corrupt trust file listed as if empty")
	}
	if _, err := execute(t, "mcp", "trust", "approve", "docs", "--config", cfg); err == nil {
		t.Fatal("approval written over a corrupt trust file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt trust file was modified: %q, %v", after, err)
	}
}
