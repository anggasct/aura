package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

const (
	liveEventsSession   = "sess-live"
	liveEventsTurn      = "turn-live"
	liveEventsPrincipal = "friend"
)

func TestBuildRelaysAnEffectfulToolRequestToTheTurnSubscriber(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	model := newStubModel(t, stubHoldsItsAnswer)
	persona := strings.ReplaceAll(compositionPersonaYAML, "[read_file]", "[read_file, write_file]")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), stubModelsYAML(model.url), persona)
	seedConfiguredStorage(t, path)
	result := loadCompositionConfig(t, path)
	result.CapabilityReport = capabilitytest.Report(t, "workspace-write")
	b := newBuild(serverCompositionOptions(result, &syncBuffer{}))
	t.Cleanup(func() {
		if b.group != nil {
			_ = b.group.Stop(context.WithoutCancel(t.Context()))
		}
	})
	for _, step := range b.phases(t.Context()) {
		if err := step(); err != nil {
			t.Fatalf("build phase: %v", err)
		}
	}
	now := time.Now().UTC()
	if err := store.NewSessionService(b.db).Create(t.Context(), &store.Session{
		ID: liveEventsSession, OwnerID: principal.OwnerID, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the session: %v", err)
	}
	grant := store.StandingGrant{
		PrincipalID: liveEventsPrincipal, AgentID: "desk", ToolName: "write_file",
		GrantedBy: principal.OwnerID, GrantedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, err := store.NewStandingGrantStore(b.db).Grant(t.Context(), &grant); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := b.group.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}

	kinds := make(chan store.RuntimeEvent, 64)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer close(kinds)
		for ev, err := range b.engine.Run(ctx, &turn.TurnRequest{
			TurnID: liveEventsTurn, SessionID: liveEventsSession, PrincipalID: liveEventsPrincipal,
			PrincipalClass: principal.ClassTrusted, Origin: turn.OriginInternal, AgentID: "desk",
			Parts: []runtimeingress.InputPart{{Text: "hi"}},
		}) {
			if err != nil {
				return
			}
			kinds <- ev
		}
	}()
	model.awaitFirstCall(t)

	if _, err := b.builtin.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-write", TurnID: liveEventsTurn, SessionID: liveEventsSession, PrincipalID: liveEventsPrincipal,
		PrincipalClass: principal.ClassTrusted, AgentID: "desk", ToolName: "write_file", ToolVersion: "v1",
		Arguments: json.RawMessage(`{"path":"out.txt","content":"hello"}`), Capabilities: []string{"workspace-write"},
		Trust: "owner_input", Deadline: time.Now().Add(time.Minute), IdempotencyKey: "live/write",
		EventInvocation: "inv-live", EventBranch: "main", EventAuthor: "agent",
	}); err != nil {
		t.Fatalf("Execute write_file: %v", err)
	}

	timeout := time.After(compositionServerWait)
	for requested := false; !requested; {
		select {
		case ev, ok := <-kinds:
			if !ok {
				t.Fatal("the turn stream ended before tool.requested was relayed")
			}
			requested = ev.Kind == turn.EventKindToolRequested && ev.TurnID == liveEventsTurn
		case <-timeout:
			t.Fatal("tool.requested did not reach the turn subscriber while the model was held")
		}
	}
	if got := terminalTurnKind(t, b.db, liveEventsTurn); got != "" {
		t.Fatalf("the turn ended with %q before the model answered", got)
	}
	model.open()
}
