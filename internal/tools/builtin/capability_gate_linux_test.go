//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
	"github.com/anggasct/aura/internal/toolbroker"
)

type gateFixture struct {
	executor  *Executor
	workspace string
	prompts   int
}

func newGateFixture(t *testing.T, report capability.Report, approve bool) *gateFixture {
	t.Helper()
	fixture := &gateFixture{workspace: t.TempDir()}
	if err := os.WriteFile(filepath.Join(fixture.workspace, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	dataRoot := t.TempDir()
	storetest.Seed(t, filepath.Join(dataRoot, "aura.db"))
	db, err := store.OpenDB(context.Background(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	session := store.Session{ID: "session-1", OwnerID: "owner-1"}
	if err := store.NewSessionService(db).Create(context.Background(), &session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = fixture.workspace
	cfg.Storage.Path = dataRoot
	executor, err := New(&cfg, db, dataRoot, report, nil, nil, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
		fixture.prompts++
		return approve, nil
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fixture.executor = executor
	return fixture
}

func (f *gateFixture) call(tool, arguments string, capabilities ...string) (json.RawMessage, error) {
	return f.executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-" + tool, TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: tool, ToolVersion: "v1", Arguments: json.RawMessage(arguments),
		Capabilities: capabilities, Trust: string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "test/" + tool, EventSequence: 1, EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	})
}

func offered(executor *Executor) []string {
	definitions := executor.Definitions()
	names := make([]string, 0, len(definitions))
	for i := range definitions {
		names = append(names, definitions[i].Name)
	}
	slices.Sort(names)
	return names
}

var ungatedTools = []string{"email_send", "handoff_to_owner", "kb_search", "workflow_cancel", "workflow_list", "workflow_propose", "workflow_save_routine", "workflow_start", "workflow_status"}

func TestExecutorOffersOnlyToolsWhoseCapabilityIsActive(t *testing.T) {
	cases := []struct {
		name    string
		enabled []string
		gated   []string
	}{
		{name: "no capability enabled", enabled: nil, gated: nil},
		{name: "workspace read", enabled: []string{"workspace-read"}, gated: []string{"list_dir", "read_file"}},
		{name: "workspace write", enabled: []string{"workspace-write"}, gated: []string{"write_file"}},
		{name: "exec", enabled: []string{"exec-linux"}, gated: []string{"exec"}},
		{name: "public web", enabled: []string{"public-web"}, gated: []string{"web_fetch"}},
		{name: "provider search", enabled: []string{"provider-search"}, gated: []string{"web_search"}},
		{name: "code interpreter gates no builtin tool", enabled: []string{"code-interpreter"}, gated: nil},
		{name: "everything", enabled: []string{"exec-linux", "workspace-read", "workspace-write", "public-web", "provider-search"}, gated: []string{"exec", "list_dir", "read_file", "web_fetch", "web_search", "write_file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGateFixture(t, capabilitytest.Report(t, tc.enabled...), true)
			want := slices.Sorted(slices.Values(append(slices.Clone(ungatedTools), tc.gated...)))
			if got := offered(fixture.executor); !slices.Equal(got, want) {
				t.Fatalf("offered = %v, want %v", got, want)
			}
		})
	}
}

func TestExecutorWithTheZeroReportOffersOnlyUngatedTools(t *testing.T) {
	fixture := newGateFixture(t, capability.Report{}, true)
	if got, want := offered(fixture.executor), ungatedTools; !slices.Equal(got, want) {
		t.Fatalf("offered = %v, want %v", got, want)
	}
}

func TestExecutorOmitsEnabledCapabilitiesThatAreNotAvailable(t *testing.T) {
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatalf("BuiltinRegistry: %v", err)
	}
	build, err := capability.NewBuild(string(capability.ProfileExecLinux), []string{"exec-linux", "workspace-read"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	report, _ := registry.Resolve(build, []string{"exec-linux", "workspace-read"}, capability.Dependencies{})
	fixture := newGateFixture(t, report, true)
	want := slices.Sorted(slices.Values(append(slices.Clone(ungatedTools), "list_dir", "read_file")))
	if got := offered(fixture.executor); !slices.Equal(got, want) {
		t.Fatalf("offered = %v, want %v", got, want)
	}
}

func TestStaticCatalogIgnoresTheCapabilitySet(t *testing.T) {
	if got, want := len(DefinitionNames()), 15; got != want {
		t.Fatalf("DefinitionNames() = %d names, want %d", got, want)
	}
	if got, want := len(CatalogDefinitions()), 15; got != want {
		t.Fatalf("CatalogDefinitions() = %d entries, want %d", got, want)
	}
}

func TestDirectCallsToUnavailableToolsAreDeniedWithoutEffect(t *testing.T) {
	cases := []struct {
		tool       string
		arguments  string
		capability string
		effect     func(workspace string) bool
	}{
		{tool: "exec", arguments: `{"command":["touch","exec-marker"]}`, capability: "exec-linux", effect: markerExists("exec-marker")},
		{tool: "read_file", arguments: `{"path":"note.txt"}`, capability: "workspace-read"},
		{tool: "list_dir", arguments: `{"path":"."}`, capability: "workspace-read"},
		{tool: "write_file", arguments: `{"path":"written.txt","content":"x"}`, capability: "workspace-write", effect: markerExists("written.txt")},
		{tool: "web_fetch", arguments: `{"url":"https://example.com"}`, capability: "public-web"},
		{tool: "web_search", arguments: `{"query":"aura"}`, capability: "provider-search"},
	}
	for _, tc := range cases {
		reports := map[string]capability.Report{
			"nothing enabled":    capabilitytest.Report(t),
			"zero report":        {},
			"only other enabled": capabilitytest.Report(t, otherCapability(tc.capability)),
		}
		for reportName, report := range reports {
			t.Run(tc.tool+"/"+reportName, func(t *testing.T) {
				fixture := newGateFixture(t, report, true)
				_, err := fixture.call(tc.tool, tc.arguments, tc.capability)
				if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultCapabilityUnavailable {
					t.Fatalf("call error = %v, want capability_unavailable", err)
				}
				if fixture.prompts != 0 {
					t.Fatalf("approval prompts = %d, want none", fixture.prompts)
				}
				if tc.effect != nil && tc.effect(fixture.workspace) {
					t.Fatal("the denied call left an effect in the workspace")
				}
			})
		}
	}
}

func otherCapability(name string) string {
	if name == "workspace-read" {
		return "public-web"
	}
	return "workspace-read"
}

func markerExists(name string) func(workspace string) bool {
	return func(workspace string) bool {
		_, err := os.Stat(filepath.Join(workspace, name))
		return err == nil
	}
}

func TestEnabledCapabilityRunsItsToolAsBefore(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "workspace-read"), true)
	output, err := fixture.call("read_file", `{"path":"note.txt"}`, "workspace-read")
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(output, &result); err != nil || result.Content != "hello" {
		t.Fatalf("read_file output = %s, %v; want hello", output, err)
	}
	if fixture.prompts != 0 {
		t.Fatalf("approval prompts = %d, want none for a read", fixture.prompts)
	}
}

func TestEnabledExecStillNeedsApproval(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "exec-linux"), false)
	if !slices.Contains(offered(fixture.executor), "exec") {
		t.Fatal("exec is not offered with exec-linux enabled")
	}
	_, err := fixture.call("exec", `{"command":["touch","exec-marker"]}`, "exec-linux")
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultApprovalRequired {
		t.Fatalf("exec error = %v, want approval_required", err)
	}
	if fixture.prompts != 1 {
		t.Fatalf("approval prompts = %d, want one", fixture.prompts)
	}
	if markerExists("exec-marker")(fixture.workspace) {
		t.Fatal("a rejected exec left an effect")
	}
}

func TestRequestClaimingACapabilityTheHostLacksIsDenied(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "workspace-read"), true)
	_, err := fixture.call("read_file", `{"path":"note.txt"}`, "workspace-read", "exec-linux")
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultCapabilityUnavailable {
		t.Fatalf("read_file error = %v, want capability_unavailable", err)
	}
}
