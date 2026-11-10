package runtimeengine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
)

type knowledgePersonas struct {
	stubPersonas
	lists map[string][]string
	asked []string
}

func (k *knowledgePersonas) Knowledge(agentID string, class principal.Class) []string {
	k.asked = append(k.asked, agentID+"|"+string(class))
	return slices.Clone(k.lists[agentID+"|"+string(class)])
}

func newKnowledgePersonas() *knowledgePersonas {
	return &knowledgePersonas{
		stubPersonas: stubPersonas{"main": personaFixture("main", "soul-main")},
		lists: map[string][]string{
			"main|owner":   {"support", "hr-docs"},
			"main|trusted": {"support"},
			"main|public":  {"faq"},
		},
	}
}

func TestStampPersonaPicksTheKnowledgeListByClass(t *testing.T) {
	tests := map[string]struct {
		class principal.Class
		want  []string
	}{
		"owner":   {class: principal.ClassOwner, want: []string{"support", "hr-docs"}},
		"trusted": {class: principal.ClassTrusted, want: []string{"support"}},
		"public":  {class: principal.ClassPublic, want: []string{"faq"}},
		"missing": {class: "", want: nil},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			engine, _, _ := newTestRuntime(t, Config{DefaultAgentID: "main", Personas: newKnowledgePersonas()}, turn.NewFakeExecutor(nil))
			req := sampleRequest("session-a", "turn-0")
			req.PrincipalClass = test.class
			if err := engine.stampPersona(req); err != nil {
				t.Fatalf("stampPersona: %v", err)
			}
			if !slices.Equal(req.Knowledge, test.want) {
				t.Errorf("Knowledge = %v, want %v", req.Knowledge, test.want)
			}
		})
	}
}

func TestStampPersonaOverwritesAKnowledgeListSetByTheCaller(t *testing.T) {
	engine, _, _ := newTestRuntime(t, Config{DefaultAgentID: "main", Personas: newKnowledgePersonas()}, turn.NewFakeExecutor(nil))
	req := sampleRequest("session-a", "turn-0")
	req.PrincipalClass = principal.ClassPublic
	req.Knowledge = []string{"hr-docs", "payroll"}
	if err := engine.stampPersona(req); err != nil {
		t.Fatalf("stampPersona: %v", err)
	}
	if !slices.Equal(req.Knowledge, []string{"faq"}) {
		t.Errorf("Knowledge = %v, want the persona's public list", req.Knowledge)
	}
}

func TestStampPersonaKeepsTheAcceptedListOfAResumedTurn(t *testing.T) {
	personas := newKnowledgePersonas()
	engine, _, _ := newTestRuntime(t, Config{DefaultAgentID: "main", Personas: personas}, turn.NewFakeExecutor(nil))
	req := sampleRequest("session-a", "turn-0")
	req.PrincipalClass = principal.ClassOwner
	accepted := personaFixture("main", "soul-main")
	req.Persona = &accepted
	req.Knowledge = []string{"accepted-list"}
	desc := turnDescriptorFromRequest(req)
	resumed := turnRequestFromDescriptor(&desc)
	if err := engine.stampPersona(resumed); err != nil {
		t.Fatalf("stampPersona: %v", err)
	}
	if !slices.Equal(resumed.Knowledge, []string{"accepted-list"}) || len(personas.asked) != 0 {
		t.Errorf("Knowledge = %v after asking %v, want the accepted list untouched", resumed.Knowledge, personas.asked)
	}
}

func TestKnowledgeSurvivesTheDescriptorRoundTripAndIsOmittedWhenEmpty(t *testing.T) {
	req := sampleRequest("session-a", "turn-0")
	req.Knowledge = []string{"support", "hr-docs"}
	payload, err := json.Marshal(turnDescriptorFromRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var stored runtimesessions.Descriptor
	if err := json.Unmarshal(payload, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := turnRequestFromDescriptor(&stored).Knowledge; !slices.Equal(got, req.Knowledge) {
		t.Errorf("round trip = %v, want %v", got, req.Knowledge)
	}
	stored.Knowledge[0] = "mutated"
	if req.Knowledge[0] != "support" {
		t.Error("the descriptor shares the request's list")
	}

	empty, err := json.Marshal(turnDescriptorFromRequest(sampleRequest("session-a", "turn-1")))
	if err != nil || strings.Contains(string(empty), "knowledge") {
		t.Errorf("an empty list must not appear in the descriptor: %s %v", empty, err)
	}
	legacy := []byte(`{"turn_id":"t","session_id":"s","principal_id":"p","origin":"terminal","parts":[],"idempotency_key":"k","max_tokens":0,"max_cost":0,"trace_parent":"","agent_id":"main"}`)
	var old runtimesessions.Descriptor
	if err := json.Unmarshal(legacy, &old); err != nil || len(turnRequestFromDescriptor(&old).Knowledge) != 0 {
		t.Errorf("a descriptor without the field must replay with none: %v", err)
	}
}

func TestAcceptedTurnReachesTheExecutorWithItsKnowledgeList(t *testing.T) {
	executor := &recordingExecutor{inner: turn.NewFakeExecutor([]turn.FakeStep{jsonStep(turn.EventKindMessageCompleted)})}
	engine, db, _ := newTestRuntime(t, Config{DefaultAgentID: "main", Personas: newKnowledgePersonas()}, executor)
	mustCreateSession(t, db, "session-a")
	req := sampleRequest("session-a", "turn-k")
	req.PrincipalClass = principal.ClassOwner
	if _, err := collect(t, engine, req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := executor.requests()
	if len(seen) != 1 || !slices.Equal(seen[0].Knowledge, []string{"support", "hr-docs"}) {
		t.Fatalf("executor requests = %+v", seen)
	}
}
