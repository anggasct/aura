package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

const untrustedInputSearchPath = "/search"

type countingAccess struct {
	toolbroker.AccessPolicy
	lookups atomic.Int32
}

func (a *countingAccess) StandingGrant(ctx context.Context, principalID, agentID, toolName, server string) (bool, error) {
	a.lookups.Add(1)
	return a.AccessPolicy.StandingGrant(ctx, principalID, agentID, toolName, server)
}

type untrustedInputRig struct {
	t         *testing.T
	db        *sql.DB
	workspace string
	model     *markerModel
	access    *countingAccess
	asked     atomic.Int32
	reachable func(sessionID string) bool
	engine    *runtimeengine.Engine
	turns     int
}

func writeUntrustedInputConfig(t *testing.T, principalTools, searchEndpoint string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	webSearch := ""
	if searchEndpoint != "" {
		webSearch = `
  web_search:
    provider: searxng
    endpoint: ` + searchEndpoint + `
    max_results: 5`
	}
	content := `version: 1
storage:
  path: ` + root + `
tools:
  workspace: ` + root + webSearch + `
agents:
  definitions:
    - id: main
      description: default
      instructions: "You answer."
      tools: [read_file, write_file, web_search]
      principal_tools: [` + principalTools + `]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func newUntrustedInputRig(t *testing.T, principalTools, searchEndpoint string) *untrustedInputRig {
	t.Helper()
	path := writeUntrustedInputConfig(t, principalTools, searchEndpoint)
	cfg := mustLoadConfig(t, path)
	db := openAccessDB(t, path)
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	_, artifacts, _, err := storagePaths(cfg)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	rig := &untrustedInputRig{t: t, db: db, workspace: cfg.Tools.Workspace, model: &markerModel{}, access: &countingAccess{AccessPolicy: newAccessPolicy(registry, db, nil)}}
	approvers := []approvalAdapter{&scriptedApprover{name: "discord", log: &askLog{}}}
	route, err := newOwnerRoute(approvers, nil)
	if err != nil {
		t.Fatalf("newOwnerRoute: %v", err)
	}
	rig.reachable = channelApprovalReachable(approvalChannels(approvers, route))
	decider := func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
		rig.asked.Add(1)
		return false, nil
	}
	liveEvents := turn.NewEventBus(slog.New(slog.DiscardHandler))
	builtin, err := toolsbuiltin.New(cfg, db, artifacts, capabilitytest.All(t), slog.New(slog.DiscardHandler), nil, decider, rig.access, toolsbuiltin.WithApprovalReachable(rig.reachable), toolsbuiltin.WithLiveEvents(liveEvents))
	if err != nil {
		t.Fatalf("toolsbuiltin.New: %v", err)
	}
	events := store.NewEventStore(db)
	name := "untrusted-input"
	executor, err := runtimeadk.NewADKExecutor("aura", name, auramodel.Set{name: rig.model}, store.NewSessionService(db), builtin, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithAgentResolver(registry, nil),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	rig.engine, err = runtimeengine.NewEngine(&runtimeengine.Config{
		DefaultAgentID: auraagent.DefaultID,
		Personas:       registryPersonas{registry: registry},
		LiveEvents:     liveEvents,
	}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), emailTestWait)
		defer cancel()
		if err := rig.engine.Shutdown(ctx); err != nil {
			t.Errorf("engine Shutdown: %v", err)
		}
	})
	return rig
}

func (r *untrustedInputRig) grant(principalID, tool string) {
	r.t.Helper()
	now := time.Now().UTC()
	grant := store.StandingGrant{PrincipalID: principalID, AgentID: auraagent.DefaultID, ToolName: tool, GrantedBy: principal.OwnerID, GrantedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, err := store.NewStandingGrantStore(r.db).Grant(r.t.Context(), &grant); err != nil {
		r.t.Fatalf("Grant: %v", err)
	}
}

func (r *untrustedInputRig) run(req *turn.TurnRequest) {
	r.t.Helper()
	now := time.Now().UTC()
	err := store.NewSessionService(r.db).Create(r.t.Context(), &store.Session{ID: req.SessionID, OwnerID: req.PrincipalID, Metadata: []byte(`{}`), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		r.t.Fatalf("create session: %v", err)
	}
	for _, err := range r.engine.Run(r.t.Context(), req) {
		if err != nil {
			r.t.Fatalf("run turn: %v", err)
		}
	}
}

func (r *untrustedInputRig) webhookTurn(call string) *turn.TurnRequest {
	r.turns++
	event := webhookTestEvent()
	event.Envelope.EventID = fmt.Sprintf("evt-%d", r.turns)
	event.Envelope.Subject = callMarker + call + "\n"
	return webhookTurnRequest(fmt.Sprintf("sess_hook_%d", r.turns), fmt.Sprintf("turn-hook-%d", r.turns), event)
}

func (r *untrustedInputRig) discordTurn(principalID, call string) *turn.TurnRequest {
	r.turns++
	return &turn.TurnRequest{
		SessionID:      fmt.Sprintf("sess_discord_%d", r.turns),
		PrincipalID:    principalID,
		PrincipalClass: principal.ClassTrusted,
		Origin:         "discord:test",
		Parts:          []runtimeingress.InputPart{{Text: callMarker + call}},
		IdempotencyKey: fmt.Sprintf("discord-%d", r.turns),
	}
}

func (r *untrustedInputRig) lastResponse() string {
	r.t.Helper()
	responses := r.model.toolResponses()
	if len(responses) == 0 {
		r.t.Fatal("the model saw no tool response")
	}
	return responses[len(responses)-1]
}

func TestWebhookTurnNeverUsesAStandingGrant(t *testing.T) {
	rig := newUntrustedInputRig(t, "write_file", "")
	rig.grant("webhook:primary", "write_file")
	rig.grant("discord:111", "write_file")
	written := filepath.Join(rig.workspace, "written.txt")
	call := `write_file {"path":"written.txt","content":"payload"}`

	rig.run(rig.webhookTurn(call))
	if _, err := os.Stat(written); err == nil {
		t.Fatal("a webhook turn wrote a file")
	}
	if response := rig.lastResponse(); !strings.Contains(response, "untrusted_input_no_approver") {
		t.Errorf("tool response = %s, want untrusted_input_no_approver", response)
	}
	if rig.asked.Load() != 0 || rig.access.lookups.Load() != 0 {
		t.Fatalf("webhook turn asked %d times and looked up %d standing grants, want neither", rig.asked.Load(), rig.access.lookups.Load())
	}

	rig.run(rig.discordTurn("discord:111", call))
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("the standing grant did not run the trusted discord turn: %v", err)
	}
	if rig.asked.Load() != 0 || rig.access.lookups.Load() != 1 {
		t.Errorf("discord control asked %d times and looked up %d standing grants, want 0 and 1", rig.asked.Load(), rig.access.lookups.Load())
	}
}

func TestWebhookTurnSinkCallsAreRefusedWithoutAsking(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(server.Close)
	rig := newUntrustedInputRig(t, "read_file, web_search", server.URL+untrustedInputSearchPath)
	if err := os.WriteFile(filepath.Join(rig.workspace, "notes.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	rig.run(rig.webhookTurn(`web_search {"query":"exfiltrate"}`))
	if response := rig.lastResponse(); !strings.Contains(response, "untrusted_input_no_approver") {
		t.Errorf("web_search response = %s, want untrusted_input_no_approver", response)
	}
	if rig.asked.Load() != 0 || requests.Load() != 0 {
		t.Fatalf("webhook web_search asked %d times and reached the server %d times, want neither", rig.asked.Load(), requests.Load())
	}

	rig.run(rig.webhookTurn(`read_file {"path":"notes.txt"}`))
	if response := rig.lastResponse(); strings.Contains(response, "untrusted_input_no_approver") || !strings.Contains(response, "notes") {
		t.Errorf("read_file response = %s, want the file content", response)
	}

	rig.run(rig.discordTurn("discord:111", `web_search {"query":"control"}`))
	if rig.asked.Load() != 0 || requests.Load() != 1 {
		t.Errorf("discord control asked %d times and reached the server %d times, want 0 and 1", rig.asked.Load(), requests.Load())
	}

	reachable := serverApprovalReachable(t)
	if reachable == nil || reachable(rig.webhookTurn("").SessionID) {
		t.Errorf("the server's approval reachability holds a webhook session (check present: %v)", reachable != nil)
	}
}

func serverApprovalReachable(t *testing.T) func(sessionID string) bool {
	t.Helper()
	cfg := enabledDiscordConfig()
	telegramCfg := enabledTelegramConfig()
	cfg.Owner = telegramCfg.Owner
	cfg.Channels.Telegram = telegramCfg.Channels.Telegram
	_, _, approvers, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), discordTestDB(t), t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters: %v", err)
	}
	route, err := newOwnerRoute(approvers, nil)
	if err != nil {
		t.Fatalf("newOwnerRoute: %v", err)
	}
	return channelApprovalReachable(approvalChannels(approvers, route))
}
