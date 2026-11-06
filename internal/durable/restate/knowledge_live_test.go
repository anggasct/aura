//go:build durable

package restate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type liveKnowledgeStub struct {
	mu       sync.Mutex
	evidence runtime.KnowledgeEvidence
	err      error
	calls    int
	scopes   [][]string
}

func (s *liveKnowledgeStub) KnowledgeContext(_ context.Context, req *runtime.TurnRequest) (runtime.KnowledgeEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.scopes = append(s.scopes, slices.Clone(req.Knowledge))
	return s.evidence, s.err
}

func (s *liveKnowledgeStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestLiveKnowledgeSurvivesCrashOnce(t *testing.T) {
	binary := liveRestateBinary(t)
	block := runtime.KnowledgeContextStart + ": reference passages]\npassage [source=faq.md#1 trust=untrusted_external] heading: \"Refunds\" text: \"ORIGINAL-KNOWLEDGE\"\n" + runtime.KnowledgeContextEnd
	stub := &liveKnowledgeStub{evidence: runtime.KnowledgeEvidence{Block: block, ChunkIDs: []string{"kbc_original"}, Retrieved: 3}}
	model := &liveScriptModel{steps: []liveModelStep{{tool: "tool-a"}, {text: "done"}}}
	rig := startLiveTurnRig(t, binary, model, []liveToolDef{{name: "tool-a"}}, liveSkillWiring{knowledge: stub})
	entered := filepath.Join(t.TempDir(), "tool-a-entered")
	release := filepath.Join(t.TempDir(), "tool-a-release")
	rig.tools.gates["tool-a:entered"] = entered
	rig.tools.gates["tool-a"] = release

	req := liveTurnRequest("turn-knowledge")
	req.PrincipalClass = principal.ClassOwner
	req.Knowledge = []string{"support"}
	streamDone := make(chan error, 1)
	go func() {
		for _, err := range rig.engine.Run(context.Background(), req) {
			if err != nil {
				streamDone <- err
				return
			}
		}
		streamDone <- nil
	}()
	rig.waitFile(entered, 90*time.Second)
	stub.mu.Lock()
	stub.evidence = runtime.KnowledgeEvidence{Block: runtime.KnowledgeContextStart + "]\nCHANGED-KNOWLEDGE\n" + runtime.KnowledgeContextEnd, ChunkIDs: []string{"kbc_changed"}, Retrieved: 9}
	stub.err = errors.New("index unavailable after restart")
	stub.mu.Unlock()
	rig.crash()
	rig.restart()
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("write release flag: %v", err)
	}
	events := rig.waitTerminal("turn-knowledge", 120*time.Second)
	if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("terminal = %q, want turn.completed", last.Kind)
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stream never terminated")
	}

	if calls := stub.callCount(); calls != 1 {
		t.Fatalf("knowledge provider calls = %d, want the retrieval journaled once across the restart", calls)
	}
	if len(stub.scopes) != 1 || !slices.Equal(stub.scopes[0], []string{"support"}) {
		t.Fatalf("provider saw scopes %v, want the stamped list rebuilt from the descriptor", stub.scopes)
	}
	var retrieved []store.RuntimeEvent
	for i := range events {
		if events[i].Kind == runtime.EventKindKnowledgeRetrieved {
			retrieved = append(retrieved, events[i])
		}
	}
	if len(retrieved) != 1 {
		t.Fatalf("kb.retrieved.v1 events = %d, want 1", len(retrieved))
	}
	var payload struct {
		ChunkIDs []string `json:"chunk_ids"`
		Outcome  string   `json:"outcome"`
		Counts   struct {
			Retrieved int `json:"retrieved"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(retrieved[0].Payload, &payload); err != nil || payload.Outcome != "ok" || !slices.Equal(payload.ChunkIDs, []string{"kbc_original"}) || payload.Counts.Retrieved != 3 {
		t.Fatalf("retrieved payload = %s (%v), want the journaled original", retrieved[0].Payload, err)
	}
	if strings.Contains(string(retrieved[0].Payload), "ORIGINAL-KNOWLEDGE") {
		t.Fatalf("the event payload carries passage text: %s", retrieved[0].Payload)
	}
	requests := model.seenRequests()
	if len(requests) < 2 {
		t.Fatalf("model requests = %d, want one before and one after the restart", len(requests))
	}
	for i, seen := range requests {
		if !strings.Contains(seen, "ORIGINAL-KNOWLEDGE") || strings.Contains(seen, "CHANGED-KNOWLEDGE") {
			t.Errorf("model request %d lacks the journaled block or holds the changed one:\n%s", i, seen)
		}
	}
	for i := range events {
		if strings.Contains(string(events[i].Payload), "ORIGINAL-KNOWLEDGE") {
			t.Errorf("event %s persisted the knowledge text", events[i].Kind)
		}
	}
}

func TestLiveKnowledgeFailureKeepsTheTurn(t *testing.T) {
	binary := liveRestateBinary(t)
	stub := &liveKnowledgeStub{err: errors.New("index unavailable")}
	model := &liveScriptModel{steps: []liveModelStep{{text: "answer"}}}
	rig := startLiveTurnRig(t, binary, model, []liveToolDef{{name: "tool-a"}}, liveSkillWiring{knowledge: stub})

	req := liveTurnRequest("turn-knowledge-failure")
	req.PrincipalClass = principal.ClassOwner
	req.Knowledge = []string{"support"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range rig.engine.Run(context.Background(), req) {
		}
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("stream never terminated")
	}
	events := rig.waitTerminal("turn-knowledge-failure", 30*time.Second)
	if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("terminal = %q, want turn.completed", last.Kind)
	}
	if model.callCount() != 1 || strings.Contains(strings.Join(model.seenRequests(), "\n"), runtime.KnowledgeContextStart) {
		t.Fatalf("model calls = %d, want one call without any block", model.callCount())
	}
	if got := rig.countKind("turn-knowledge-failure", runtime.EventKindKnowledgeRetrieved); got != 1 {
		t.Fatalf("kb.retrieved.v1 events = %d, want 1", got)
	}
}
