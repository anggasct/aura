package runtimeengine

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

func callersContext() *turn.UntrustedRecall {
	return &turn.UntrustedRecall{
		Query:   "what did we decide",
		Summary: "two earlier messages",
		Trust:   approval.TrustUntrustedExternal,
		Documents: []turn.UntrustedDocument{
			{ID: "doc-1", SessionID: "session-old", FromSequence: 3, ToSequence: 4, Trust: approval.TrustUntrustedExternal, Content: "role=user text=\"first\""},
			{ID: "doc-2", SessionID: "session-old", FromSequence: 5, ToSequence: 9, Trust: approval.TrustDerivedUntrusted, Content: "role=assistant text=\"second\""},
		},
	}
}

func requestWithEveryFieldSet() *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID:           "turn-full",
		SessionID:        "session-full",
		PrincipalID:      "discord:601",
		PrincipalClass:   principal.ClassTrusted,
		Origin:           turn.OriginHTTP,
		Parts:            []runtimeingress.InputPart{{Text: "hello", Image: json.RawMessage(`{"mime":"image/png","data":"aGk="}`)}},
		ReplyContext:     json.RawMessage(`{"channel":"7001"}`),
		UntrustedContext: callersContext(),
		RequireRecall:    true,
		IdempotencyKey:   "key-full",
		Deadline:         time.Date(2031, time.March, 4, 5, 6, 7, 0, time.UTC),
		Budget:           turn.Budget{MaxTokens: 1000, MaxCost: 2.5},
		TraceParent:      "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		AgentID:          "main",
		Persona:          &turn.PersonaFingerprint{AgentID: "main", SoulSHA256: "soul", ToolsSHA256: "tools", ModelRoute: "primary"},
		Skill:            &turn.SkillPin{SkillID: "skill-1", Digest: "digest", InvocationID: "invocation", PolicyVersion: "policy"},
		StreamPartial:    true,
		Knowledge:        []string{"support", "hr-docs"},
	}
}

func requireEveryFieldSet(t *testing.T, path string, value reflect.Value) {
	t.Helper()
	if value.Type() == reflect.TypeFor[time.Time]() {
		if value.IsZero() {
			t.Errorf("%s is not set in the fixture", path)
		}
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for i := range value.NumField() {
			requireEveryFieldSet(t, path+"."+value.Type().Field(i).Name, value.Field(i))
		}
	case reflect.Pointer:
		if value.IsNil() {
			t.Errorf("%s is not set in the fixture", path)
			return
		}
		requireEveryFieldSet(t, path, value.Elem())
	case reflect.Slice:
		if value.Len() == 0 {
			t.Errorf("%s is not set in the fixture", path)
			return
		}
		for i := range value.Len() {
			requireEveryFieldSet(t, path+"[]", value.Index(i))
		}
	default:
		if value.IsZero() {
			t.Errorf("%s is not set in the fixture", path)
		}
	}
}

func descriptorThroughJSON(t *testing.T, req *turn.TurnRequest) (stored runtimesessions.Descriptor, keys map[string]json.RawMessage) {
	t.Helper()
	encoded, err := json.Marshal(turnDescriptorFromRequest(req))
	if err != nil {
		t.Fatalf("encode descriptor: %v", err)
	}
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("decode descriptor keys: %v", err)
	}
	return stored, keys
}

func TestTurnDescriptorCarriesEveryRequestField(t *testing.T) {
	t.Run("every field survives the stored descriptor", func(t *testing.T) {
		req := requestWithEveryFieldSet()
		requireEveryFieldSet(t, "TurnRequest", reflect.ValueOf(req))
		stored, _ := descriptorThroughJSON(t, req)
		if restored := turnRequestFromDescriptor(&stored); !reflect.DeepEqual(restored, req) {
			t.Fatalf("restored request = %+v, want %+v", restored, req)
		}
	})
	t.Run("a request without context stores no context", func(t *testing.T) {
		req := requestWithEveryFieldSet()
		req.UntrustedContext = nil
		req.RequireRecall = false
		stored, keys := descriptorThroughJSON(t, req)
		for _, key := range []string{"untrusted_context", "require_recall"} {
			if _, present := keys[key]; present {
				t.Errorf("descriptor JSON has the key %q", key)
			}
		}
		restored := turnRequestFromDescriptor(&stored)
		if restored.UntrustedContext != nil || restored.RequireRecall {
			t.Fatalf("restored context = %+v, require recall = %v; want none", restored.UntrustedContext, restored.RequireRecall)
		}
	})
}

func TestDurableTurnExecutesWithTheCallersContext(t *testing.T) {
	executor := &recordingExecutor{inner: turntest.NewFakeExecutor([]turntest.FakeStep{jsonStep(turn.EventKindMessageCompleted)})}
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, db, "session-context")
	req := sampleRequest("session-context", "turn-context")
	req.UntrustedContext = callersContext()
	req.RequireRecall = true
	if _, err := collect(t, engine, req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := executor.requests()
	if len(seen) != 1 {
		t.Fatalf("executor requests = %d, want 1", len(seen))
	}
	if !reflect.DeepEqual(seen[0].UntrustedContext, callersContext()) {
		t.Errorf("executor context = %+v, want %+v", seen[0].UntrustedContext, callersContext())
	}
	if !seen[0].RequireRecall {
		t.Error("executor request lost RequireRecall")
	}
}
