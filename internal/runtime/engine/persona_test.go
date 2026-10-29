package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/runtime"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
)

type recordingExecutor struct {
	inner *runtime.FakeExecutor
	mu    sync.Mutex
	seen  []runtime.TurnRequest
}

func (r *recordingExecutor) Execute(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	r.mu.Lock()
	r.seen = append(r.seen, *req)
	r.mu.Unlock()
	return r.inner.Execute(ctx, req)
}

func (r *recordingExecutor) requests() []runtime.TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.TurnRequest(nil), r.seen...)
}

func acceptedPayloadOf(t *testing.T, db *sql.DB, turnID string) []byte {
	t.Helper()
	var payload []byte
	if err := db.QueryRowContext(context.Background(), `SELECT payload_json FROM runtime_event WHERE turn_id = ? AND kind = ?`, turnID, runtime.EventKindTurnAccepted).Scan(&payload); err != nil {
		t.Fatalf("read accepted payload: %v", err)
	}
	return payload
}

type stubPersonas map[string]runtime.PersonaFingerprint

func (s stubPersonas) Fingerprint(agentID string) (runtime.PersonaFingerprint, bool) {
	fingerprint, ok := s[agentID]
	return fingerprint, ok
}

func personaFixture(agentID, soul string) runtime.PersonaFingerprint {
	return runtime.PersonaFingerprint{AgentID: agentID, SoulSHA256: soul, ToolsSHA256: "tools-" + agentID, ModelRoute: "primary"}
}

func acceptedPersona(t *testing.T, payload []byte) (string, *runtime.PersonaFingerprint) {
	t.Helper()
	var decoded struct {
		AgentID string                      `json:"agent_id"`
		Persona *runtime.PersonaFingerprint `json:"persona_fingerprint"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode accepted payload %s: %v", payload, err)
	}
	return decoded.AgentID, decoded.Persona
}

func TestAcceptStampsEnvelopePersonaAndFingerprint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &recordingExecutor{inner: runtime.NewFakeExecutor([]runtime.FakeStep{jsonStep(runtime.EventKindMessageCompleted)})}
		personas := stubPersonas{"main": personaFixture("main", "soul-main"), "kuma": personaFixture("kuma", "soul-kuma")}
		engine, db, _ := newTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4, DefaultAgentID: "main", Personas: personas}, executor)
		env := sampleEnvelope("conv-1", "msg-1")
		env.AgentID = "kuma"
		env.ConversationID = "kuma/telegram/conv-1"
		mustCreateSession(t, db, env.ConversationID)

		ref, err := engine.Accept(context.Background(), env)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		waitFor(t, func() bool { return terminalCount(t, db, ref.TurnID) == 1 })
		if got := executor.requests(); len(got) != 1 || got[0].AgentID != "kuma" || got[0].Persona == nil || got[0].Persona.SoulSHA256 != "soul-kuma" {
			t.Fatalf("executor requests = %+v, want one kuma turn with its fingerprint", got)
		}
		agentID, persona := acceptedPersona(t, acceptedPayloadOf(t, db, ref.TurnID))
		if agentID != "kuma" || persona == nil || *persona != personas["kuma"] {
			t.Fatalf("accepted persona = %q %+v, want kuma fingerprint", agentID, persona)
		}
	})
}

func TestRunDefaultsPersonaAndRejectsUnknown(t *testing.T) {
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{jsonStep(runtime.EventKindMessageCompleted)})
	personas := stubPersonas{"main": personaFixture("main", "soul-main")}
	engine, db, _ := newTestRuntime(t, Config{DefaultAgentID: "main", Personas: personas}, executor)
	mustCreateSession(t, db, "session-a")

	events, err := collect(t, engine, sampleRequest("session-a", "turn-default"))
	if err != nil {
		t.Fatalf("default turn: %v", err)
	}
	if agentID, persona := acceptedPersona(t, events[0].Payload); agentID != "main" || persona == nil || persona.SoulSHA256 != "soul-main" {
		t.Fatalf("default accepted payload = %s, want main fingerprint", events[0].Payload)
	}

	req := sampleRequest("session-a", "turn-unknown")
	req.AgentID = "ghost"
	_, err = collect(t, engine, req)
	if code, ok := runtime.CodeOf(err); !ok || code != runtime.ErrorCodePersonaNotFound {
		t.Fatalf("unknown persona error = %v, want persona_not_found", err)
	}
	if got := eventCountByKind(t, db, "turn-unknown", runtime.EventKindTurnAccepted); got != 0 {
		t.Fatalf("unknown persona journaled %d accepted events", got)
	}
}

func TestDurableDescriptorKeepsAcceptedPersona(t *testing.T) {
	req := sampleRequest("session-a", "turn-0")
	req.AgentID = "kuma"
	accepted := personaFixture("kuma", "soul-v1")
	req.Persona = &accepted
	payload, err := json.Marshal(turnDescriptorFromRequest(req))
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	var desc struct {
		AgentID string                      `json:"agent_id"`
		Persona *runtime.PersonaFingerprint `json:"persona_fingerprint"`
	}
	if err := json.Unmarshal(payload, &desc); err != nil || desc.Persona == nil {
		t.Fatalf("descriptor %s does not persist the fingerprint: %v", payload, err)
	}

	engine, _, _ := newTestRuntime(t, Config{DefaultAgentID: "main", Personas: stubPersonas{"kuma": personaFixture("kuma", "soul-v2")}}, runtime.NewFakeExecutor(nil))
	var stored runtimesessions.Descriptor
	if err := json.Unmarshal(payload, &stored); err != nil {
		t.Fatalf("unmarshal descriptor: %v", err)
	}
	resumed := turnRequestFromDescriptor(&stored)
	if err := engine.stampPersona(resumed); err != nil {
		t.Fatalf("stampPersona: %v", err)
	}
	if resumed.AgentID != "kuma" || resumed.Persona == nil || resumed.Persona.SoulSHA256 != "soul-v1" {
		t.Fatalf("resumed persona = %q %+v, want the accepted soul-v1 fingerprint", resumed.AgentID, resumed.Persona)
	}
}
