package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func writePrincipalMatrixConfig(t *testing.T, searchEndpoint string, withKnowledge, includeEgressWithKnowledge bool) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	principalTools := "read_file, write_file, web_search, web_fetch"
	publicTools := "read_file, web_search, web_fetch"
	knowledgeBlock := ""
	if withKnowledge {
		if !includeEgressWithKnowledge {
			principalTools = "read_file, write_file"
			publicTools = "read_file"
		}
		knowledgeBlock = `
      knowledge: [notes]
      principal_knowledge: [notes]
      public_knowledge: [notes]`
	}
	content := `version: 1
storage:
  path: ` + root + `
tools:
  workspace: ` + t.TempDir() + `
  web_search:
    provider: searxng
    endpoint: ` + searchEndpoint + `
    max_results: 5
agents:
  definitions:
    - id: main
      description: default
      instructions: "You answer."
      tools: [read_file, write_file, web_search, web_fetch]
      principal_tools: [` + principalTools + `]
      public_tools: [` + publicTools + `]` + knowledgeBlock + `
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !includeEgressWithKnowledge {
		seedConfiguredStorage(t, path)
	}
	return path
}

type principalMatrixRig struct {
	t         *testing.T
	rawDB     *untrustedInputRig
	engine    *runtimeengine.Engine
	model     *markerModel
	access    *countingAccess
	asked     atomic.Int32
	workspace string
	turns     int
}

func newPrincipalMatrixRig(t *testing.T, searchEndpoint string, withKnowledge bool) *principalMatrixRig {
	t.Helper()
	path := writePrincipalMatrixConfig(t, searchEndpoint, withKnowledge, false)
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
	rig := &principalMatrixRig{
		t:         t,
		rawDB:     &untrustedInputRig{db: db},
		model:     &markerModel{},
		access:    &countingAccess{AccessPolicy: newAccessPolicy(registry, db, nil)},
		workspace: cfg.Tools.Workspace,
	}
	reachable := func(string) bool { return true }
	decider := func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
		rig.asked.Add(1)
		return false, nil
	}
	liveEvents := turn.NewEventBus(slog.New(slog.DiscardHandler))
	builtin, err := newTestToolExecutor(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: artifacts,
		logger:       slog.New(slog.DiscardHandler),
		wiring:       &toolWiring{decider: decider, access: rig.access, reachable: reachable},
		events:       liveEvents,
	})
	if err != nil {
		t.Fatalf("newTestToolExecutor: %v", err)
	}
	events := turnstore.NewEventStore(db)
	name := "principal-matrix"
	executor, err := runtimeadk.NewADKExecutor("aura", name, auramodel.Set{name: rig.model}, turnstore.NewSessionService(db), builtin, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(builtin),
		runtimeadk.WithAgentResolver(registry, nil),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	rig.engine, err = runtimeengine.New(&runtimeengine.Deps{
		Events:         events,
		Dedupe:         turnstore.NewDedupeStore(db),
		Executor:       executor,
		Config:         runtimeengine.Default(),
		DefaultAgentID: auraagent.DefaultID,
		Personas:       auraagent.RegistryPersonas{Registry: registry},
		LiveEvents:     liveEvents,
	})
	if err != nil {
		t.Fatalf("runtimeengine.New: %v", err)
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

func (r *principalMatrixRig) grantStanding(principalID, tool string) {
	r.t.Helper()
	now := time.Now().UTC()
	grant := principal.StandingGrant{
		PrincipalID: principalID,
		AgentID:     auraagent.DefaultID,
		ToolName:    tool,
		GrantedBy:   principal.OwnerID,
		GrantedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
	}
	if _, err := principal.NewStandingGrantStore(r.rawDB.db).Grant(r.t.Context(), &grant); err != nil {
		r.t.Fatalf("Grant: %v", err)
	}
}

func (r *principalMatrixRig) runCall(class principal.Class, principalID, call string, relayed bool) (response string, asked, lookups int32) {
	r.t.Helper()
	r.turns++
	sessionID := "sess-matrix-" + string(class) + "-" + time.Now().Format("150405.000000000") + "-" + string(rune('a'+r.turns))
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(r.rawDB.db).Create(r.t.Context(), &turn.Session{
		ID: sessionID, OwnerID: principalID, Metadata: []byte(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		r.t.Fatalf("create session: %v", err)
	}
	part := runtimeingress.InputPart{Text: callMarker + call}
	if relayed {
		part.Source = turn.PartSourceRelayed
		part.Detail = "discord forward"
	}
	askedBefore := r.asked.Load()
	lookupsBefore := r.access.lookups.Load()
	for _, err := range r.engine.Run(r.t.Context(), &turn.TurnRequest{
		SessionID:      sessionID,
		PrincipalID:    principalID,
		PrincipalClass: class,
		Origin:         "discord:test",
		Parts:          []runtimeingress.InputPart{part},
		IdempotencyKey: sessionID,
	}) {
		if err != nil {
			r.t.Fatalf("engine.Run: %v", err)
		}
	}
	responses := r.model.toolResponses()
	if len(responses) == 0 {
		r.t.Fatal("model saw no tool response")
	}
	return responses[len(responses)-1], r.asked.Load() - askedBefore, r.access.lookups.Load() - lookupsBefore
}

func TestOtherPrincipalsKeepTheStricterRules(t *testing.T) {
	var searchHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		searchHits.Add(1)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(server.Close)

	t.Run("trusted principal without knowledge scope", func(t *testing.T) {
		rig := newPrincipalMatrixRig(t, server.URL+"/search", false)
		rig.grantStanding("discord:111", "write_file")

		beforeHits := searchHits.Load()
		resp, asked, _ := rig.runCall(principal.ClassTrusted, "discord:111", `web_search {"query":"weather today"}`, false)
		if asked != 0 || searchHits.Load()-beforeHits != 1 || strings.Contains(resp, "untrusted_input") {
			t.Errorf("clean trusted web_search: resp=%s asked=%d hits=%d, want ran without card", resp, asked, searchHits.Load()-beforeHits)
		}

		beforeHits = searchHits.Load()
		resp, asked, _ = rig.runCall(principal.ClassTrusted, "discord:111", `web_search {"query":"weather today"}`, true)
		if asked != 1 || searchHits.Load()-beforeHits != 0 {
			t.Errorf("tainted trusted web_search: resp=%s asked=%d hits=%d, want 1 card and 0 server hits (no search relief for trusted)", resp, asked, searchHits.Load()-beforeHits)
		}

		resp, asked, _ = rig.runCall(principal.ClassTrusted, "discord:111", `web_fetch {"url":"https://example.com/page"}`, true)
		if asked != 1 {
			t.Errorf("tainted trusted web_fetch: resp=%s asked=%d, want 1 card (no fetch relief for trusted)", resp, asked)
		}

		_, asked, lookups := rig.runCall(principal.ClassTrusted, "discord:111", `write_file {"path":"clean.txt","content":"ok"}`, false)
		if asked != 0 || lookups != 1 {
			t.Errorf("clean trusted standing grant: asked=%d lookups=%d, want 0 and 1", asked, lookups)
		}
		if _, err := os.Stat(filepath.Join(rig.workspace, "clean.txt")); err != nil {
			t.Errorf("clean trusted standing grant did not write file: %v", err)
		}

		_, asked, lookups = rig.runCall(principal.ClassTrusted, "discord:111", `write_file {"path":"tainted.txt","content":"no"}`, true)
		if asked != 1 || lookups != 0 {
			t.Errorf("tainted trusted standing grant: asked=%d lookups=%d, want 1 card and 0 standing lookups", asked, lookups)
		}
		if _, err := os.Stat(filepath.Join(rig.workspace, "tainted.txt")); err == nil {
			t.Error("tainted trusted turn used standing grant to write file")
		}
	})

	t.Run("trusted principal with knowledge scope keeps egress forbidden and stays tainted by knowledge_scope", func(t *testing.T) {
		invalidPath := writePrincipalMatrixConfig(t, server.URL+"/search", true, true)
		if _, err := appconfig.Load(invalidPath); err == nil {
			t.Fatal("config with principal_knowledge and egress tools unexpectedly loaded")
		}

		rig := newPrincipalMatrixRig(t, server.URL+"/search", true)
		rig.grantStanding("discord:111", "write_file")

		_, asked, lookups := rig.runCall(principal.ClassTrusted, "discord:111", `write_file {"path":"kb.txt","content":"no"}`, false)
		if asked != 1 || lookups != 0 {
			t.Errorf("trusted+knowledge standing grant: asked=%d lookups=%d, want 1 card and 0 standing lookups", asked, lookups)
		}
		if _, err := os.Stat(filepath.Join(rig.workspace, "kb.txt")); err == nil {
			t.Error("trusted+knowledge turn used standing grant to write file")
		}
	})

	t.Run("public principal without knowledge scope", func(t *testing.T) {
		rig := newPrincipalMatrixRig(t, server.URL+"/search", false)

		beforeHits := searchHits.Load()
		resp, asked, _ := rig.runCall(principal.ClassPublic, "discord:222", `web_search {"query":"public weather"}`, false)
		if asked != 0 || searchHits.Load()-beforeHits != 1 || strings.Contains(resp, "untrusted_input") {
			t.Errorf("clean public web_search: resp=%s asked=%d hits=%d, want ran without card", resp, asked, searchHits.Load()-beforeHits)
		}

		beforeHits = searchHits.Load()
		resp, asked, _ = rig.runCall(principal.ClassPublic, "discord:222", `web_search {"query":"public weather"}`, true)
		if asked != 0 || searchHits.Load()-beforeHits != 0 || !strings.Contains(resp, "untrusted_input_no_approver") {
			t.Errorf("tainted public web_search: resp=%s asked=%d hits=%d, want refused untrusted_input_no_approver with 0 cards", resp, asked, searchHits.Load()-beforeHits)
		}

		resp, asked, _ = rig.runCall(principal.ClassPublic, "discord:222", `web_fetch {"url":"https://example.com/pub"}`, true)
		if asked != 0 || !strings.Contains(resp, "untrusted_input_no_approver") {
			t.Errorf("tainted public web_fetch: resp=%s asked=%d, want refused untrusted_input_no_approver with 0 cards", resp, asked)
		}
	})

	t.Run("public principal with knowledge scope stays tainted by knowledge_scope", func(t *testing.T) {
		ownerReq := &turn.TurnRequest{Origin: "discord:test", PrincipalClass: principal.ClassOwner, Knowledge: []string{"notes"}}
		if mark, tainted := ownerReq.AdmissionTaint(); tainted {
			t.Errorf("owner with knowledge scope admission taint = %+v, want untainted", mark)
		}
		publicReq := &turn.TurnRequest{Origin: "discord:test", PrincipalClass: principal.ClassPublic, Knowledge: []string{"notes"}}
		if mark, tainted := publicReq.AdmissionTaint(); !tainted || mark.Source != turn.TaintSourceKnowledgeScope {
			t.Errorf("public with knowledge scope admission taint = %+v, %v; want knowledge_scope", mark, tainted)
		}
		trustedReq := &turn.TurnRequest{Origin: "discord:test", PrincipalClass: principal.ClassTrusted, Knowledge: []string{"notes"}}
		if mark, tainted := trustedReq.AdmissionTaint(); !tainted || mark.Source != turn.TaintSourceKnowledgeScope {
			t.Errorf("trusted with knowledge scope admission taint = %+v, %v; want knowledge_scope", mark, tainted)
		}
	})
}
