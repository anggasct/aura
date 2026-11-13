//go:build linux

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestToolExecutorRedactsTheGitHubTokens(t *testing.T) {
	const token = "ghp-test-secret"
	t.Setenv("AURA_TEST_GITHUB_TOKEN", token)
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("the token is "+token), 0o600); err != nil {
		t.Fatalf("write note: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Storage.Path = dir
	cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
	storetest.Seed(t, filepath.Join(dir, "aura.db"))
	db, err := store.OpenDB(t.Context(), filepath.Join(dir, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	executor, err := newToolExecutor(&toolExecutorInput{
		result: loadResultWithAll(t, &cfg), db: db, artifactRoot: filepath.Join(dir, "artifacts"), logger: slog.Default(),
		wiring: &toolWiring{},
	})
	if err != nil {
		t.Fatalf("newToolExecutor: %v", err)
	}
	output, err := executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: "read_file", ToolVersion: "v1", Arguments: json.RawMessage(`{"path":"note.txt"}`),
		Capabilities: []string{"workspace-read"}, Trust: string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "test/read", EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(string(output), token) || !strings.Contains(string(output), "the token is") {
		t.Errorf("read_file output = %s, want the note with the token redacted", output)
	}
}

func TestWorkflowToolRunnerRefusesAnUnconfiguredGitHubCredential(t *testing.T) {
	comment := func(credential string) json.RawMessage {
		return json.RawMessage(`{"repo":"org/repo","number":7,"body":"hi","credential_name":"` + credential + `"}`)
	}
	runner, db, _ := newWorkflowToolRunnerFixtureWith(t, func(cfg *config.Config) {
		cfg.Tools.GitHub.Credentials = []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
		cfg.Agents.Definitions = append(cfg.Agents.Definitions, config.AgentDefinition{
			ID: "reviewer", Description: "d", Instructions: "i", Tools: []string{github.ToolComment},
		})
	})
	asReviewer := func(runID string) context.Context {
		call := ownerStepCall(runID, "step-1", 0)
		call.Origin.AgentID = "reviewer"
		return stepContextFor(t, db, call)
	}
	_, err := runner.Invoke(asReviewer("run-other"), github.ToolComment, comment("other"))
	if err == nil || !strings.Contains(err.Error(), "not a configured github credential") {
		t.Errorf("unconfigured credential err = %v, want not a configured github credential", err)
	}
	_, err = runner.Invoke(asReviewer("run-ci"), github.ToolComment, comment("ci"))
	if err == nil || !strings.Contains(err.Error(), "github credential is unavailable") {
		t.Errorf("configured credential err = %v, want github credential is unavailable", err)
	}
	bare, bareDB, _ := newWorkflowToolRunnerFixture(t)
	_, err = bare.Invoke(stepCallContext(t, bareDB), github.ToolComment, comment("ci"))
	if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeCapabilityExceeded {
		t.Errorf("no credentials err = %v, want %s", err, workflow.ErrorCodeCapabilityExceeded)
	}
}

var providerSafeToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type gitHubToolRig struct {
	credentials []config.GitHubCredential
	wiring      toolWiring
	access      func(db *sql.DB) toolbroker.AccessPolicy
}

func (r *gitHubToolRig) build(t *testing.T, report capability.Report) (*toolsbuiltin.Executor, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Tools.Workspace = filepath.Join(dir, "workspace")
	cfg.Storage.Path = dir
	cfg.Tools.GitHub.Credentials = r.credentials
	storetest.Seed(t, filepath.Join(dir, "aura.db"))
	db, err := store.OpenDB(t.Context(), filepath.Join(dir, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	wiring := r.wiring
	if r.access != nil {
		wiring.access = r.access(db)
	}
	executor, err := newToolExecutor(&toolExecutorInput{
		result: &config.LoadResult{Config: &cfg, CapabilityReport: report}, db: db, artifactRoot: filepath.Join(dir, "artifacts"),
		logger: slog.New(slog.DiscardHandler), wiring: &wiring,
	})
	if err != nil {
		t.Fatalf("newToolExecutor: %v", err)
	}
	return executor, db
}

func oneCredential() []config.GitHubCredential {
	return []config.GitHubCredential{{Name: "ci", Ref: "env://AURA_TEST_GITHUB_TOKEN"}}
}

func approveAll(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil }

func gitHubCommentRequest(class principal.Class, credential string) *runtimeadk.BuiltinToolRequest {
	return &runtimeadk.BuiltinToolRequest{
		RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1", PrincipalClass: class,
		ToolName: github.ToolComment, ToolVersion: github.ToolVersion,
		Arguments:    json.RawMessage(`{"repo":"org/repo","number":7,"body":"hi","credential_name":"` + credential + `"}`),
		Capabilities: []string{"provider-github"}, Trust: string(approval.TrustTrustedConfiguration),
		IdempotencyKey: "test/comment", EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	}
}

func definitionByName(definitions []runtimeadk.BuiltinToolDefinition, name string) (runtimeadk.BuiltinToolDefinition, bool) {
	index := slices.IndexFunc(definitions, func(d runtimeadk.BuiltinToolDefinition) bool { return d.Name == name })
	if index < 0 {
		return runtimeadk.BuiltinToolDefinition{}, false
	}
	return definitions[index], true
}

func TestToolExecutorRegistersTheGitHubTools(t *testing.T) {
	rig := &gitHubToolRig{credentials: oneCredential(), wiring: toolWiring{decider: approveAll}}
	executor, _ := rig.build(t, capabilitytest.All(t))
	definitions := executor.Definitions()
	for _, name := range github.ToolNames() {
		if count := countDefinitions(definitions, name); count != 1 {
			t.Fatalf("%s listed %d times, want once", name, count)
		}
		definition, _ := definitionByName(definitions, name)
		if !definition.OptIn {
			t.Errorf("%s is not opt-in", name)
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if got := schema.Properties["credential_name"].Enum; !slices.Equal(got, []string{"ci"}) {
			t.Errorf("%s credential_name enum = %v, want [ci]", name, got)
		}
		for _, want := range []string{"credential_name", "exact", "refused"} {
			if !strings.Contains(definition.Description, want) {
				t.Errorf("%s description %q lacks %q", name, definition.Description, want)
			}
		}
	}
	_, err := executor.Execute(t.Context(), gitHubCommentRequest(principal.ClassOwner, "other"))
	if err == nil || !strings.Contains(err.Error(), "not a configured github credential") {
		t.Errorf("unconfigured credential err = %v, want not a configured github credential", err)
	}
	_, err = executor.Execute(t.Context(), gitHubCommentRequest(principal.ClassOwner, "ci"))
	if err == nil || !strings.Contains(err.Error(), "github credential is unavailable") {
		t.Errorf("configured credential err = %v, want github credential is unavailable", err)
	}

	bare := &gitHubToolRig{wiring: toolWiring{decider: approveAll}}
	executor, _ = bare.build(t, capabilitytest.All(t))
	for _, definition := range executor.Definitions() {
		if strings.HasPrefix(definition.Name, "github") {
			t.Errorf("executor without credentials lists %q", definition.Name)
		}
	}
	_, err = executor.Execute(t.Context(), gitHubCommentRequest(principal.ClassOwner, "ci"))
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultPolicyDenied || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("no credentials err = %v, want policy_denied unknown tool", err)
	}

	noTools := config.Default()
	noTools.Tools = nil
	if githubCredentials(&noTools) != nil {
		t.Errorf("githubCredentials without a tools section = %v, want nil", githubCredentials(&noTools))
	}
	_, err = newToolExecutor(&toolExecutorInput{result: loadResultWithAll(t, &noTools), db: nil, logger: slog.New(slog.DiscardHandler), wiring: &toolWiring{}})
	if err == nil || !strings.Contains(err.Error(), "tools configuration is required") {
		t.Errorf("newToolExecutor without a tools section err = %v, want tools configuration is required", err)
	}
}

func countDefinitions(definitions []runtimeadk.BuiltinToolDefinition, name string) int {
	count := 0
	for i := range definitions {
		if definitions[i].Name == name {
			count++
		}
	}
	return count
}

func TestBuiltinToolNamesAreProviderSafe(t *testing.T) {
	rig := &gitHubToolRig{credentials: oneCredential(), wiring: toolWiring{decider: approveAll}}
	executor, _ := rig.build(t, capabilitytest.All(t))
	definitions := executor.Definitions()
	for _, name := range github.ToolNames() {
		if _, ok := definitionByName(definitions, name); !ok {
			t.Errorf("executor lacks %s", name)
		}
	}
	for _, definition := range definitions {
		if !providerSafeToolName.MatchString(definition.Name) {
			t.Errorf("tool name %q is not provider safe", definition.Name)
		}
	}
}

func TestGitHubToolsNeedTheGitHubProviderCapability(t *testing.T) {
	all := capabilitytest.All(t)
	var others []string
	for _, status := range all.Statuses() {
		if status.Name != "provider-github" {
			others = append(others, status.Name)
		}
	}
	rig := &gitHubToolRig{credentials: oneCredential(), wiring: toolWiring{decider: approveAll}}
	executor, _ := rig.build(t, capabilitytest.Report(t, others...))
	for _, definition := range executor.Definitions() {
		if strings.HasPrefix(definition.Name, "github") {
			t.Errorf("host without provider-github lists %q", definition.Name)
		}
	}
	executor, _ = rig.build(t, all)
	for _, name := range github.ToolNames() {
		definition, ok := definitionByName(executor.Definitions(), name)
		if !ok || !slices.Equal(definition.RequiredCapabilities, []string{"provider-github"}) {
			t.Errorf("%s = %+v, %t; want it listed with provider-github required", name, definition.RequiredCapabilities, ok)
		}
	}
}

func TestSurfaceExecutorAsksApprovalForAGitHubCall(t *testing.T) {
	var prompts []*toolbroker.ApprovalPrompt
	verdict := false
	cfg := config.Default()
	cfg.Tools.GitHub.Credentials = oneCredential()
	registry, err := auraagent.Build([]config.AgentDefinition{
		{ID: "ops", Description: "d", Instructions: "i", Audience: config.AudienceTrusted, Tools: []string{"read_file", "github_comment"}, PrincipalTools: []string{"github_comment"}},
		{ID: "plain", Description: "d", Instructions: "i", Audience: config.AudienceTrusted, Tools: []string{"read_file", "github_comment"}, PrincipalTools: []string{"read_file"}},
		{ID: "lobby", Description: "d", Instructions: "i", Audience: config.AudiencePublic, Tools: []string{"read_file", "github_comment"}, PrincipalTools: []string{"github_comment"}, PublicTools: []string{"read_file"}},
	}, personaToolNames(&cfg), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var policy toolbroker.AccessPolicy
	rig := &gitHubToolRig{
		credentials: oneCredential(),
		access: func(db *sql.DB) toolbroker.AccessPolicy {
			policy = newAccessPolicy(registry, db, nil)
			return policy
		},
	}
	rig.wiring.decider = func(_ context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		prompts = append(prompts, prompt)
		return verdict, nil
	}
	executor, db := rig.build(t, capabilitytest.All(t))

	_, err = executor.Execute(t.Context(), gitHubCommentRequest(principal.ClassOwner, "ci"))
	if err == nil || strings.Contains(err.Error(), "github credential is unavailable") || len(prompts) != 1 || prompts[0].ToolName != github.ToolComment {
		t.Fatalf("denied owner call err = %v, prompts = %d; want one prompt for github_comment and no adapter run", err, len(prompts))
	}

	refused := func(class principal.Class, agentID string) {
		t.Helper()
		request := gitHubCommentRequest(class, "ci")
		request.PrincipalID, request.AgentID = "discord:111", agentID
		_, err := executor.Execute(t.Context(), request)
		if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultToolTierDenied {
			t.Errorf("%s call as %s err = %v, want tool_tier_denied", agentID, class, err)
		}
	}
	refused(principal.ClassTrusted, "plain")
	refused(principal.ClassPublic, "lobby")
	if len(prompts) != 1 {
		t.Fatalf("decider asked %d times after the refused calls, want still 1", len(prompts))
	}

	now := time.Now().UTC()
	grant := &store.StandingGrant{PrincipalID: "discord:111", AgentID: "ops", ToolName: github.ToolComment, GrantedBy: "discord:777", GrantedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, err := store.NewStandingGrantStore(db).Grant(t.Context(), grant); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if active, err := policy.StandingGrant(t.Context(), "discord:111", "ops", github.ToolComment, ""); err != nil || !active {
		t.Fatalf("standing grant active = %v, %v; want an active grant", active, err)
	}
	verdict = true
	request := gitHubCommentRequest(principal.ClassTrusted, "ci")
	request.PrincipalID, request.AgentID = "discord:111", "ops"
	_, err = executor.Execute(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "github credential is unavailable") {
		t.Fatalf("trusted call with a standing grant err = %v, want it approved per call and stopped at the unset credential", err)
	}
	if len(prompts) != 2 || prompts[1].StandingGrantAllowed {
		t.Fatalf("prompts = %d, StandingGrantAllowed = %t; want a second card that offers no standing grant", len(prompts), len(prompts) > 1 && prompts[1].StandingGrantAllowed)
	}
}

const gitHubHTTPChatConfig = `version: 1
storage:
  path: %[1]s
tools:
  workspace: %[1]s
  github:
    credentials:
      - name: ci
        ref: env://AURA_TEST_GITHUB_TOKEN
capabilities:
  enabled: [provider-github]
channels:
  http:
    enabled: true
agents:
  definitions:
%[2]s`

func newGitHubHTTPChatHarness(t *testing.T, personas string, llm adkmodel.LLM) *httpChatHarness {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(gitHubHTTPChatConfig, root, personas)), 0o600); err != nil {
		t.Fatal(err)
	}
	build, err := capability.ParseBuild("core", "provider-github", runtime.GOOS)
	if err != nil {
		t.Fatalf("ParseBuild: %v", err)
	}
	capabilities, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatalf("BuiltinRegistry: %v", err)
	}
	loaded, err := config.LoadWithOptions(path, config.LoadOptions{Build: build, Registry: capabilities})
	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	cfg := loaded.Config
	dbPath, _, _, err := storagePaths(cfg)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	storetest.Seed(t, dbPath)
	db, err := openStorage(t.Context(), cfg)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	passEvals(t, db, registry, "ops", "plain", "open")
	builtin, err := newToolExecutor(&toolExecutorInput{
		result: loadResultWithAll(t, cfg), db: db, artifactRoot: t.TempDir(), logger: slog.New(slog.DiscardHandler),
		wiring: &toolWiring{decider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return false, nil }},
	})
	if err != nil {
		t.Fatalf("newToolExecutor: %v", err)
	}
	name := "http-chat-model"
	events := store.NewEventStore(db)
	executor, err := runtimeadk.NewADKExecutor("aura", name, auramodel.Set{name: llm}, store.NewSessionService(db), events, personaEvalDenyAll{}, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithAgentResolver(registry, nil),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	engine, err := runtimeengine.NewEngine(&runtimeengine.Config{
		DefaultAgentID: auraagent.DefaultID,
		Personas:       registryPersonas{registry: registry},
	}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	server, _, err := buildHTTPChat(cfg, db, engine, registry, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildHTTPChat: %v", err)
	}
	return &httpChatHarness{t: t, config: path, cfg: cfg, baseURL: serveHTTPChat(t, server), db: db}
}

func (h *httpChatHarness) mintKey(persona, class string) string {
	h.t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := createAPIKey(h.t.Context(), cmd, h.cfg, h.db, &apiKeyCreateFlags{persona: persona, class: class, label: "test"}); err != nil {
		h.t.Fatalf("createAPIKey: %v", err)
	}
	return strings.TrimSpace(out.String())
}

const gitHubHTTPChatPersonas = `    - id: ops
      description: operations
      instructions: "You run operations."
      audience: public
      tools: [read_file, web_search, github_comment]
      principal_tools: [read_file, github_comment]
      public_tools: [web_search]
    - id: plain
      description: plain
      instructions: "You help."
      audience: trusted
      tools: [read_file]
    - id: open
      description: open
      instructions: "You help."
      audience: trusted
`

func TestHTTPChatOffersGitHubToolsOnlyToPersonasThatListThem(t *testing.T) {
	model := &personaEvalModel{}
	h := newGitHubHTTPChatHarness(t, gitHubHTTPChatPersonas, model)
	h.model = model
	cases := []struct {
		persona string
		class   string
		want    []string
	}{
		{"ops", "trusted", []string{"github_comment", "read_file"}},
		{"ops", "public", []string{"web_search"}},
		{"ops", "owner", nil},
		{"open", "owner", nil},
		{"plain", "trusted", nil},
		{"open", "trusted", nil},
	}
	for _, tc := range cases {
		t.Run(tc.persona+" "+tc.class, func(t *testing.T) {
			reply := h.post(h.mintKey(tc.persona, tc.class), chatJSON(chatMessage("user", "hello")))
			if reply.status != 200 {
				t.Fatalf("reply = %d %s", reply.status, reply.body)
			}
			request := h.lastModelRequest()
			declared := declaredTools(request)
			githubDeclared := slices.DeleteFunc(slices.Clone(declared), func(name string) bool { return !strings.HasPrefix(name, "github") })
			if tc.want == nil && len(githubDeclared) != 0 {
				t.Errorf("declared = %v, want no github tool", declared)
			}
			if tc.want != nil && !slices.Equal(declared, tc.want) {
				t.Errorf("declared = %v, want %v", declared, tc.want)
			}
			encoded, err := json.Marshal(request.Config.Tools)
			if err != nil {
				t.Fatalf("marshal tools: %v", err)
			}
			if strings.Contains(string(encoded), "env:") {
				t.Errorf("declared tools leak a reference: %s", encoded)
			}
			if slices.Contains(declared, "github_comment") && !strings.Contains(string(encoded), `"enum":["ci"]`) {
				t.Errorf("github_comment declaration lacks the credential_name enum: %s", encoded)
			}
		})
	}
}
