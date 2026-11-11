//go:build durable

package app

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

func TestDurableTurnFirstLife(t *testing.T) {
	raw := os.Getenv(firstLifeEnv)
	if raw == "" {
		t.Skip("runs only as the first life of a killed-process suite")
	}
	var plan lifePlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatalf("decode the first life plan: %v", err)
	}
	plan.Life = 1
	life := openLife(t, plan)
	if life.scenario.firstLife != nil {
		life.scenario.firstLife(t, life)
	} else {
		for i := range life.scenario.requests {
			life.submit(t, &life.scenario.requests[i])
		}
	}
	<-t.Context().Done()
}

func TestDurableTurnToolsSurviveAKilledProcess(t *testing.T) {
	suite := startKilledSuite(t, "tools")
	suite.child.waitKillPoint(t, "tool-b is entered", suite.ledger.exists(t, "tool-b.entered"))
	if n := len(suite.ledger.records(t, modelCalls)); n != 2 {
		t.Fatalf("model calls at the kill = %d, want 2", n)
	}

	life := suite.resume(t)
	life.requireActivity(t, "turn-1", 0)
	suite.release(t, "tool-b")

	suite.requireResumed(t, life)
	suite.ledger.requireCount(t, modelCalls, 3)
	suite.ledger.requireCount(t, executions("tool-a"), 1)
	suite.ledger.requireCount(t, executions("tool-b"), 1)
}

func TestDurableTurnApprovalSurvivesAKilledProcess(t *testing.T) {
	suite := startKilledSuite(t, "approval")
	var approvalID string
	suite.child.waitKillPoint(t, "the approval wait is recorded", func() bool {
		approvalID = requestedApproval(t, turnEvents(t, suite.db, "turn-1"))
		return approvalID != "" && hasWaitRow(t, suite)
	})

	life := suite.resume(t)
	decision := durable.ResolveApprovalRequest{ApprovalID: approvalID, Payload: []byte(`{"approved":true}`)}
	if err := life.runtime.ResolveApproval(t.Context(), decision); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	suite.requireResumed(t, life)
	if n := countKind(turnEvents(t, suite.db, "turn-1"), turn.EventKindApprovalRequired); n != 1 {
		t.Errorf("approval.required events = %d, want 1", n)
	}
	suite.ledger.requireCount(t, executions("tool-c"), 1)
	err := life.runtime.ResolveApproval(t.Context(), decision)
	if code, ok := sqlite.CodeOf(err); !ok || code != sqlite.ErrorCodeApprovalConsumed {
		t.Errorf("second ResolveApproval = %v, want the code %s", err, sqlite.ErrorCodeApprovalConsumed)
	}
}

func requestedApproval(t *testing.T, events []store.RuntimeEvent) string {
	t.Helper()
	for i := range events {
		if events[i].Kind != turn.EventKindApprovalRequired {
			continue
		}
		var payload struct {
			ApprovalID string `json:"approval_id"`
		}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			t.Fatalf("decode the approval event: %v", err)
		}
		return payload.ApprovalID
	}
	return ""
}

func hasWaitRow(t *testing.T, suite *killedSuite) bool {
	t.Helper()
	var waits int
	if err := suite.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_wait WHERE run_key = ?`, "turn-1").Scan(&waits); err != nil {
		t.Fatalf("read the waits of turn-1: %v", err)
	}
	return waits > 0
}

func TestDurableTurnSkillActivatesOnceAcrossAKilledProcess(t *testing.T) {
	suite := startKilledSuite(t, "skill")
	suite.child.waitKillPoint(t, "tool-a is entered", suite.ledger.exists(t, "tool-a.entered"))

	life := suite.resume(t)
	suite.release(t, "tool-a")

	suite.requireResumed(t, life)
	var activations []turn.SkillActivation
	for _, ev := range turnEvents(t, suite.db, "turn-1") {
		if ev.Kind != turn.EventKindSkillActivated {
			continue
		}
		var activation turn.SkillActivation
		if err := json.Unmarshal(ev.Payload, &activation); err != nil {
			t.Fatalf("decode the skill activation: %v", err)
		}
		activations = append(activations, activation)
	}
	if len(activations) != 1 {
		t.Fatalf("skill.activated events = %d, want 1", len(activations))
	}
	if got := activations[0]; got.Body != originalSkillBody || got.InvocationID != pinnedInvocation || got.TurnID != "turn-1" {
		t.Errorf("skill activation = %+v, want the original body, the pin's invocation id and turn-1", got)
	}
	suite.ledger.requireCount(t, skillRenders, 1)
	suite.ledger.requireCount(t, skillContextCalls, 1)
}

type retrievedPayload struct {
	ChunkIDs []string `json:"chunk_ids"`
	Mode     string   `json:"mode"`
	Outcome  string   `json:"outcome"`
	Counts   struct {
		Retrieved int `json:"retrieved"`
	} `json:"counts"`
}

func retrievals(t *testing.T, events []store.RuntimeEvent) []retrievedPayload {
	t.Helper()
	var payloads []retrievedPayload
	for i := range events {
		if events[i].Kind != turn.EventKindKnowledgeRetrieved {
			continue
		}
		if strings.Contains(string(events[i].Payload), originalKnowledge) {
			t.Errorf("the kb.retrieved.v1 payload carries passage text: %s", events[i].Payload)
		}
		var payload retrievedPayload
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			t.Fatalf("decode the kb.retrieved.v1 payload: %v", err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func TestDurableTurnKnowledgeIsRetrievedOnceAcrossAKilledProcess(t *testing.T) {
	suite := startKilledSuite(t, "knowledge")
	suite.child.waitKillPoint(t, "tool-a is entered", suite.ledger.exists(t, "tool-a.entered"))

	life := suite.resume(t)
	suite.release(t, "tool-a")

	suite.requireResumed(t, life)
	calls := suite.ledger.records(t, knowledgeCalls)
	if len(calls) != 1 || calls[0].Text != `["support"]` {
		t.Errorf("knowledge provider calls = %+v, want one call that saw the scope [\"support\"]", calls)
	}
	events := turnEvents(t, suite.db, "turn-1")
	retrieved := retrievals(t, events)
	if len(retrieved) != 1 {
		t.Fatalf("kb.retrieved.v1 events = %d, want 1", len(retrieved))
	}
	if got := retrieved[0]; got.Outcome != "ok" || !slices.Equal(got.ChunkIDs, []string{originalChunk}) || got.Counts.Retrieved != knowledgeRetrievals {
		t.Errorf("kb.retrieved.v1 payload = %+v, want the recorded original retrieval", got)
	}
	requests := suite.ledger.records(t, modelCalls)
	if len(requests) < 2 {
		t.Fatalf("model requests = %d, want at least 2", len(requests))
	}
	for i, rec := range requests {
		if !strings.Contains(rec.Text, originalKnowledge) || strings.Contains(rec.Text, changedKnowledge) {
			t.Errorf("model request %d lacks the recorded knowledge block or holds the changed one:\n%s", i+1, rec.Text)
		}
	}
	for i := range events {
		if strings.Contains(string(events[i].Payload), originalKnowledge) {
			t.Errorf("event %s of kind %s stores the knowledge text", events[i].ID, events[i].Kind)
		}
	}
}

func TestDurableTurnKnowledgeReplayMakesNoEmbeddingCall(t *testing.T) {
	suite := startKilledSuite(t, "embedding")
	suite.child.waitKillPoint(t, "tool-a is entered", suite.ledger.exists(t, "tool-a.entered"))

	life := suite.resume(t)
	suite.release(t, "tool-a")

	suite.requireResumed(t, life)
	suite.ledger.requireCount(t, embeddingCalls, 1)
	suite.ledger.requireCount(t, lexicalSearches, 1)
	for i, rec := range suite.ledger.records(t, modelCalls) {
		if !strings.Contains(rec.Text, semanticKnowledge) || !strings.Contains(rec.Text, lexicalKnowledge) {
			t.Errorf("model request %d lacks the recorded hybrid block:\n%s", i+1, rec.Text)
		}
	}
	retrieved := retrievals(t, turnEvents(t, suite.db, "turn-1"))
	if len(retrieved) != 1 {
		t.Fatalf("kb.retrieved.v1 events = %d, want 1", len(retrieved))
	}
	if got := retrieved[0]; got.Mode != knowledge.ModeHybrid || got.Outcome != "ok" {
		t.Errorf("kb.retrieved.v1 payload = %+v, want the recorded hybrid retrieval", got)
	}
}

func TestDurableTurnQueuedBehindAKilledTurnRunsAfterIt(t *testing.T) {
	suite := startKilledSuite(t, "queued")
	suite.child.waitKillPoint(t, "turn-2 is queued behind turn-1", suite.ledger.exists(t, queuedMarker))

	life := suite.resume(t)
	life.requireActivity(t, "turn-1", 0)
	status, err := life.sessions.Status(t.Context(), suiteSession)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Active == nil || status.Active.Descriptor.TurnID != "turn-1" || status.QueueDepth != 1 {
		t.Fatalf("store status = %+v, want turn-1 active and a queue depth of 1", status)
	}
	suite.release(t, "tool-a")

	suite.requireResumed(t, life)
	firstTerminals := terminalEvents(turnEvents(t, suite.db, "turn-1"))
	if len(firstTerminals) != 1 {
		t.Fatalf("terminal events of turn-1 = %d, want 1", len(firstTerminals))
	}
	for _, ev := range turnEvents(t, suite.db, "turn-2") {
		if ev.Kind == turn.EventKindTurnAccepted {
			continue
		}
		if ev.Sequence <= firstTerminals[0].Sequence {
			t.Errorf("turn-2 event %s has the sequence %d, want it after the terminal event of turn-1 at %d", ev.Kind, ev.Sequence, firstTerminals[0].Sequence)
		}
		break
	}
	suite.ledger.requireCount(t, modelCalls, 3)
	suite.ledger.requireCount(t, executions("tool-a"), 1)
	status, err = life.sessions.Status(t.Context(), suiteSession)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Active != nil || status.ActiveTurnID != "" || status.QueueDepth != 0 {
		t.Errorf("store status = %+v, want no active turn and an empty queue", status)
	}
}

func TestDurableTurnFailedToolIsNotRunAgainAfterAKilledProcess(t *testing.T) {
	suite := startKilledSuite(t, "failed-tool")
	suite.child.waitKillPoint(t, "tool-b is entered", suite.ledger.exists(t, "tool-b.entered"))

	life := suite.resume(t)
	suite.release(t, "tool-b")

	suite.requireResumed(t, life)
	suite.ledger.requireCount(t, executions("tool-a"), 1)
	suite.ledger.requireCount(t, executions("tool-b"), 1)
	suite.ledger.requireCount(t, modelCalls, 3)
}
