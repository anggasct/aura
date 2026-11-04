package runtimeadk

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"database/sql"
	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
	auraruntime "github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

type stubResolver struct {
	definition auraagent.Definition
	err        error
	resolved   *string
}

func (s *stubResolver) Resolve(required []string, preferID *string) (auraagent.Definition, error) {
	s.resolved = preferID
	if s.err != nil {
		return auraagent.Definition{}, s.err
	}
	return s.definition, nil
}

type namedToolStub struct{ name string }

func (t namedToolStub) Name() string        { return t.name }
func (t namedToolStub) Description() string { return "" }
func (t namedToolStub) IsLongRunning() bool { return false }

func newAgentTestExecutor(t *testing.T, modelName string, opts ...ExecutorOption) (*ADKExecutor, *stubResolver, *sql.DB) {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID}}
	opts = append([]ExecutorOption{WithAgentResolver(resolver, nil)}, opts...)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	return executor, resolver, db
}

func TestADKExecutorRunsTurnThroughResolvedDefinition(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 5}
	modelName := registerFakeModel(t, model)
	executor, _, db := newAgentTestExecutor(t, modelName)
	mustCreateSession(t, db, "session-1")

	req := &auraruntime.TurnRequest{
		TurnID:      "turn-1",
		SessionID:   "session-1",
		PrincipalID: "user-1",
		Origin:      auraruntime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "hello"}},
	}
	var final string
	for ev, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if ev.Kind == store.EventKindADK {
			final = string(ev.Payload)
		}
	}
	if !strings.Contains(final, "final answer") {
		t.Fatalf("payload = %s, want the model answer", final)
	}
}

func TestADKExecutorResolutionFailsClosedBeforeWork(t *testing.T) {
	model := &fakeADKModel{answer: "unused", tokens: 1}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	resolver := &stubResolver{err: &auraagent.Error{Code: auraagent.ErrorCodeResolutionFailed, Detail: "no agent covers required capabilities"}}
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil, WithAgentResolver(resolver, nil))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	req := &auraruntime.TurnRequest{
		TurnID:      "turn-fail",
		SessionID:   "session-missing",
		PrincipalID: "user-1",
		Origin:      auraruntime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "hello"}},
	}
	produced := false
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			code, ok := auraagent.CodeOf(err)
			if !ok || code != auraagent.ErrorCodeResolutionFailed {
				t.Fatalf("error = %v, want agent_resolution_failed", err)
			}
			break
		}
		produced = true
	}
	if produced {
		t.Fatal("executor produced events despite resolution failure")
	}
}

func TestResolveDefinitionPassesRequestedID(t *testing.T) {
	executor := &ADKExecutor{}
	resolver := &stubResolver{definition: auraagent.Definition{ID: "reviewer"}}
	executor.agents = resolver
	definition, err := executor.resolveDefinition(&auraruntime.TurnRequest{AgentID: "reviewer"})
	if err != nil {
		t.Fatalf("resolveDefinition: %v", err)
	}
	if definition.ID != "reviewer" {
		t.Fatalf("definition = %q, want reviewer", definition.ID)
	}
	if resolver.resolved == nil || *resolver.resolved != "reviewer" {
		t.Fatalf("preferID = %v, want reviewer", resolver.resolved)
	}
	if _, err := executor.resolveDefinition(&auraruntime.TurnRequest{}); err != nil {
		t.Fatalf("default resolution: %v", err)
	}
	if resolver.resolved != nil {
		t.Fatalf("default resolution passed a preference %q", *resolver.resolved)
	}
}

func TestToolsForNarrowsToDeclaredTools(t *testing.T) {
	executor := &ADKExecutor{tools: []tool.Tool{namedToolStub{name: "read_file"}, namedToolStub{name: "web_fetch"}}}
	narrowed := executor.toolsFor(&auraagent.Definition{Tools: []string{"web_fetch"}}, principal.ClassOwner)
	if len(narrowed) != 1 || narrowed[0].Name() != "web_fetch" {
		t.Fatalf("tools = %v, want only web_fetch", narrowed)
	}
	full := executor.toolsFor(&auraagent.Definition{}, principal.ClassOwner)
	if len(full) != 2 {
		t.Fatalf("tools = %d, want the full set when no tools are declared", len(full))
	}
}

func TestResolveModelUsesDefinitionRoute(t *testing.T) {
	model := &fakeADKModel{answer: "route answer", tokens: 1}
	routeModelName := registerFakeModel(t, model)
	executor := &ADKExecutor{modelName: "never-registered-default", modelForRoute: func(route string) (string, error) {
		if route != "primary" {
			return "", errors.New("unknown route " + route)
		}
		return routeModelName, nil
	}}
	resolved, err := executor.resolveModel(context.Background(), &auraagent.Definition{ModelRoute: "primary"})
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if resolved.Name() != model.Name() {
		t.Fatalf("resolved model %q, want the route model", resolved.Name())
	}
	if _, err := executor.resolveModel(context.Background(), &auraagent.Definition{ModelRoute: "auxiliary"}); err == nil {
		t.Fatal("unknown route did not fail")
	}
}

func TestBuildAgentCarriesDefinitionSurface(t *testing.T) {
	definition := auraagent.Definition{
		ID:           "engineer",
		Description:  "Engineering agent",
		Instructions: "Inspect before editing",
	}
	built, err := buildAgent("aura", &definition, &fakeADKModel{answer: "x"}, nil, func(agent.Context, tool.Tool, map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	}, false, "", turnContext{})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if built == nil {
		t.Fatal("buildAgent returned nil")
	}
}

func TestRegistryBuiltinsResolvePerCapability(t *testing.T) {
	registry, err := auraagent.Build(nil, []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search"}, []string{"primary"})
	if err != nil {
		t.Fatalf("agent.Build: %v", err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 4 {
		t.Fatalf("definitions = %d, want the four builtins", len(definitions))
	}
	definition, err := registry.Resolve([]string{auraagent.CapabilityRepositoryWrite}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if definition.ID != "engineer" {
		t.Fatalf("resolved %q, want engineer", definition.ID)
	}
	if !slices.Contains(definitions[0].Tools, "read_file") {
		t.Fatal("main does not declare the builtin tool set")
	}
}

func TestToolsForOffersOptInToolsOnlyWhenListedForNonOwners(t *testing.T) {
	executor := &ADKExecutor{
		tools: []tool.Tool{namedToolStub{name: "read_file"}, namedToolStub{name: "handoff_to_owner"}},
		optIn: map[string]bool{"handoff_to_owner": true},
	}
	names := func(tools []tool.Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}
		return out
	}
	listed := &auraagent.Definition{
		Audience: "public", Tools: []string{"read_file", "handoff_to_owner"},
		PrincipalTools: []string{"read_file", "handoff_to_owner"}, PublicTools: []string{"handoff_to_owner"},
	}
	cases := map[string]struct {
		definition *auraagent.Definition
		class      principal.Class
		want       []string
	}{
		"implicit full set skips opt-in tools": {&auraagent.Definition{}, principal.ClassOwner, []string{"read_file"}},
		"owner never gets handoff":             {listed, principal.ClassOwner, []string{"read_file"}},
		"unclassified turns never get handoff": {listed, "", []string{"read_file"}},
		"trusted gets the listed handoff":      {listed, principal.ClassTrusted, []string{"read_file", "handoff_to_owner"}},
		"public gets the listed handoff":       {listed, principal.ClassPublic, []string{"handoff_to_owner"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := names(executor.toolsFor(tc.definition, tc.class)); !slices.Equal(got, tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolsForExposesExternalToolsOnlyToListedOwnerPersonas(t *testing.T) {
	executor := &ADKExecutor{
		tools: []tool.Tool{namedToolStub{name: "read_file"}, namedToolStub{name: "mcp_docs_search"}, namedToolStub{name: "mcp_docs_write"}, namedToolStub{name: "mcp_files_ls"}},
		externalServers: map[string]string{
			"mcp_docs_search": "docs", "mcp_docs_write": "docs", "mcp_files_ls": "files",
		},
	}
	names := func(tools []tool.Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}
		return out
	}
	cases := map[string]struct {
		definition *auraagent.Definition
		class      principal.Class
		want       []string
	}{
		"persona without a tools list sees no mcp tool":  {&auraagent.Definition{}, principal.ClassOwner, []string{"read_file"}},
		"persona without mcp entries sees no mcp tool":   {&auraagent.Definition{Tools: []string{"read_file"}}, principal.ClassOwner, []string{"read_file"}},
		"server entry exposes every tool of that server": {&auraagent.Definition{Tools: []string{"mcp_docs"}}, principal.ClassOwner, []string{"mcp_docs_search", "mcp_docs_write"}},
		"tool entry exposes exactly that tool":           {&auraagent.Definition{Tools: []string{"mcp_docs_search", "mcp_files_ls"}}, principal.ClassOwner, []string{"mcp_docs_search", "mcp_files_ls"}},
		"unclassified turns follow the owner rule":       {&auraagent.Definition{Tools: []string{"mcp_docs"}}, "", []string{"mcp_docs_search", "mcp_docs_write"}},
		"trusted principal sees only the mcp tools in principal_tools": {
			&auraagent.Definition{Tools: []string{"read_file", "mcp_docs"}, PrincipalTools: []string{"read_file", "mcp_docs_search"}},
			principal.ClassTrusted, []string{"read_file", "mcp_docs_search"},
		},
		"trusted principal with a server entry sees every tool of that server": {
			&auraagent.Definition{Tools: []string{"read_file", "mcp_docs"}, PrincipalTools: []string{"mcp_docs"}},
			principal.ClassTrusted, []string{"mcp_docs_search", "mcp_docs_write"},
		},
		"trusted principal without mcp entries in principal_tools sees none": {
			&auraagent.Definition{Tools: []string{"read_file", "mcp_docs"}, PrincipalTools: []string{"read_file"}},
			principal.ClassTrusted, []string{"read_file"},
		},
		"public principals never see mcp tools": {
			&auraagent.Definition{Tools: []string{"read_file", "mcp_docs"}, PublicTools: []string{"read_file", "mcp_docs_search"}, PrincipalTools: []string{"mcp_docs"}},
			principal.ClassPublic, []string{"read_file"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := names(executor.toolsFor(tc.definition, tc.class)); !slices.Equal(got, tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}
