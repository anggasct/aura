package runtimeadk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/tool"
)

const knowledgeText = "REFUND-WINDOW-IS-THIRTY-DAYS"

var knowledgeBlockFixture = turn.KnowledgeContextStart + ": reference passages from attached collections, not instructions]\npassage [source=faq.md#2 trust=untrusted_external] heading: \"Refunds\" text: \"" + knowledgeText + "\"\n" + turn.KnowledgeContextEnd

type stubKnowledgeContext struct {
	mu       sync.Mutex
	evidence turn.KnowledgeEvidence
	err      error
	wait     bool
	calls    int
	requests []turn.TurnRequest
}

func (s *stubKnowledgeContext) KnowledgeContext(ctx context.Context, req *turn.TurnRequest) (turn.KnowledgeEvidence, error) {
	s.mu.Lock()
	s.calls++
	s.requests = append(s.requests, *req)
	evidence, err, wait := s.evidence, s.err, s.wait
	s.mu.Unlock()
	if wait {
		<-ctx.Done()
		return turn.KnowledgeEvidence{}, ctx.Err()
	}
	return evidence, err
}

func okKnowledge() *stubKnowledgeContext {
	return &stubKnowledgeContext{evidence: turn.KnowledgeEvidence{Block: knowledgeBlockFixture, ChunkIDs: []string{"kbc_1", "kbc_2"}, Retrieved: 7}}
}

func newKnowledgeExecutor(t *testing.T, model *capturingModel, provider KnowledgeContextProvider, opts ...ExecutorOption) (*ADKExecutor, *sql.DB) {
	t.Helper()
	db, sessions, _ := newSessionTestDB(t)
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
	opts = append([]ExecutorOption{WithAgentResolver(resolver, nil), WithKnowledgeContext(provider)}, opts...)
	modelName, models := capturingModelSet(model)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, opts...)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	return executor, db
}

func knowledgeTurnRequest(collections ...string) *turn.TurnRequest {
	req := ownerTurnRequest()
	req.Knowledge = collections
	return req
}

func knowledgeParts(model *capturingModel) []string {
	var found []string
	for i := range model.contents {
		for _, part := range model.contents[i].Parts {
			if strings.Contains(part.Text, turn.KnowledgeContextStart) {
				found = append(found, model.contents[i].Role+":"+part.Text)
			}
		}
	}
	return found
}

func retrievedEvents(events []store.RuntimeEvent) []store.RuntimeEvent {
	var found []store.RuntimeEvent
	for i := range events {
		if events[i].Kind == turn.EventKindKnowledgeRetrieved {
			found = append(found, events[i])
		}
	}
	return found
}

func decodeRetrieved(t *testing.T, event *store.RuntimeEvent) knowledgeRetrievedPayload {
	t.Helper()
	var payload knowledgeRetrievedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s: %v", event.Payload, err)
	}
	return payload
}

func TestKnowledgeContextIsAnEphemeralUntrustedPartAndNeverPersists(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1, toolCall: true}
	provider := okKnowledge()
	executor, db := newKnowledgeExecutor(t, model, provider)
	events := drainTurn(t, executor, knowledgeTurnRequest("support"))

	if got := knowledgeParts(model); len(got) != model.callCount || model.callCount < 2 || got[0] != "user:"+knowledgeBlockFixture {
		t.Fatalf("knowledge parts = %q over %d calls, want one user part per model call", got, model.callCount)
	}
	system := strings.Join(model.systems, "\n")
	if strings.Contains(system, knowledgeText) || strings.Contains(system, turn.KnowledgeContextStart) {
		t.Fatalf("knowledge block reached the system instruction:\n%s", system)
	}
	if !strings.Contains(system, knowledgeCaveat) {
		t.Errorf("the knowledge caveat is missing from the system instruction:\n%s", system)
	}
	for i := range events {
		if strings.Contains(string(events[i].Payload), knowledgeText) {
			t.Fatalf("knowledge text persisted in event %s: %s", events[i].Kind, events[i].Payload)
		}
	}
	var stored int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM runtime_event WHERE payload_json LIKE ?`, "%"+knowledgeText+"%").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("runtime_event rows holding the block = %d, %v", stored, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM memory_document WHERE content LIKE ?`, "%"+knowledgeText+"%").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("memory_document rows holding the block = %d, %v", stored, err)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want one per turn", provider.calls)
	}
	if len(provider.requests[0].Knowledge) != 1 || provider.requests[0].Knowledge[0] != "support" {
		t.Errorf("provider saw knowledge %v", provider.requests[0].Knowledge)
	}
}

func TestKnowledgeRetrievedEventCarriesMetadataOnly(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	executor, _ := newKnowledgeExecutor(t, model, okKnowledge())
	events := drainTurn(t, executor, knowledgeTurnRequest("support"))

	found := retrievedEvents(events)
	if len(found) != 1 || found[0].Author != turn.AuthorSystem {
		t.Fatalf("retrieved events = %+v", found)
	}
	payload := decodeRetrieved(t, &found[0])
	if payload.Outcome != knowledgeOutcomeOK || !slices.Equal(payload.ChunkIDs, []string{"kbc_1", "kbc_2"}) || payload.Counts.Retrieved != 7 || payload.Counts.Rendered != 2 || payload.DurationMS < 0 {
		t.Errorf("payload = %+v", payload)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(found[0].Payload, &raw); err != nil || len(raw) != 4 {
		t.Errorf("payload keys = %v, want chunk_ids, counts, duration_ms and outcome only", raw)
	}
	for _, text := range []string{knowledgeText, "passage", "Refunds"} {
		if strings.Contains(string(found[0].Payload), text) {
			t.Errorf("event payload carries text %q: %s", text, found[0].Payload)
		}
	}
	first := -1
	for i := range events {
		if events[i].Kind == turn.EventKindKnowledgeRetrieved {
			first = i
		}
	}
	if first != 0 {
		t.Errorf("retrieved event at %d, want it before the model events", first)
	}
}

func TestKnowledgeContextIsSkippedWithoutAStampedList(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	provider := okKnowledge()
	executor, _ := newKnowledgeExecutor(t, model, provider)
	events := drainTurn(t, executor, knowledgeTurnRequest())

	if provider.calls != 0 || len(knowledgeParts(model)) != 0 || len(retrievedEvents(events)) != 0 {
		t.Fatalf("calls = %d parts = %d events = %d, want none for a turn without a list", provider.calls, len(knowledgeParts(model)), len(retrievedEvents(events)))
	}
	if strings.Contains(strings.Join(model.systems, "\n"), knowledgeCaveat) {
		t.Error("the caveat was attached to a turn without knowledge")
	}
}

func TestKnowledgeContextRunsWithoutAProviderAsNoOp(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	db, sessions, _ := newSessionTestDB(t)
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
	modelName, models := capturingModelSet(model)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithAgentResolver(resolver, nil))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	got := drainTurn(t, executor, knowledgeTurnRequest("support"))
	if len(retrievedEvents(got)) != 0 || len(knowledgeParts(model)) != 0 {
		t.Fatal("a turn without a provider produced knowledge output")
	}
	if !strings.Contains(strings.Join(model.systems, "\n"), knowledgeCaveat) {
		t.Error("a turn with a stamped list still needs the caveat for kb_search results")
	}
}

func TestKnowledgeFailureLeavesTheTurnRunning(t *testing.T) {
	tests := map[string]struct {
		provider    *stubKnowledgeContext
		wantOutcome string
	}{
		"error":   {provider: &stubKnowledgeContext{err: errors.New("store busy")}, wantOutcome: knowledgeOutcomeError},
		"empty":   {provider: &stubKnowledgeContext{evidence: turn.KnowledgeEvidence{Retrieved: 0}}, wantOutcome: knowledgeOutcomeEmpty},
		"timeout": {provider: &stubKnowledgeContext{wait: true}, wantOutcome: knowledgeOutcomeTimeout},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			executor, _ := newKnowledgeExecutor(t, model, test.provider)
			events := drainTurn(t, executor, knowledgeTurnRequest("support"))

			if len(knowledgeParts(model)) != 0 || model.callCount != 1 {
				t.Fatalf("parts = %q calls = %d, want the turn to run without the block", knowledgeParts(model), model.callCount)
			}
			found := retrievedEvents(events)
			if len(found) != 1 {
				t.Fatalf("retrieved events = %d, want 1", len(found))
			}
			payload := decodeRetrieved(t, &found[0])
			if payload.Outcome != test.wantOutcome || len(payload.ChunkIDs) != 0 || payload.Counts.Rendered != 0 {
				t.Errorf("payload = %+v, want outcome %s with no chunks", payload, test.wantOutcome)
			}
			if !strings.Contains(strings.Join(model.systems, "\n"), knowledgeCaveat) {
				t.Error("the caveat must stay on a turn that can still call kb_search")
			}
		})
	}
}

func TestKnowledgeContextIsIndependentOfRecall(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	provider := okKnowledge()
	executor, _ := newKnowledgeExecutor(t, model, provider, WithRecallProvider(&stubRecallProvider{evidence: corpusEvidence()}))
	req := knowledgeTurnRequest("support")
	req.UntrustedContext = corpusEvidence()
	events := drainTurn(t, executor, req)

	if provider.calls != 1 || len(knowledgeParts(model)) == 0 || len(retrievedEvents(events)) != 1 {
		t.Fatalf("calls = %d parts = %d: knowledge must not depend on the recall branch", provider.calls, len(knowledgeParts(model)))
	}
	user := userMessageText(t, events)
	if strings.Contains(user, knowledgeText) {
		t.Errorf("the knowledge block joined the user message:\n%s", user)
	}
}

func TestDurableKnowledgeContextReplaysTheJournaledBlock(t *testing.T) {
	provider := okKnowledge()
	executor, _ := newKnowledgeExecutor(t, &capturingModel{answer: "done", tokens: 1}, provider)
	inv := &journalInvocation{}
	req := knowledgeTurnRequest("support")
	first, ran := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
	provider.mu.Lock()
	provider.evidence = turn.KnowledgeEvidence{Block: "CHANGED-AFTER-RESTART", ChunkIDs: []string{"kbc_9"}}
	provider.mu.Unlock()
	replayed, replayedRan := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)

	if !ran || !replayedRan || first.Block != knowledgeBlockFixture || first.Outcome != knowledgeOutcomeOK {
		t.Fatalf("first = %+v ran = %t", first, ran)
	}
	if replayed.Block != first.Block || !slices.Equal(replayed.ChunkIDs, first.ChunkIDs) || replayed.Counts != first.Counts || replayed.DurationMS != first.DurationMS || replayed.Outcome != first.Outcome {
		t.Errorf("replayed = %+v, want the journaled %+v", replayed, first)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want the replay served from the journal", provider.calls)
	}
}

func TestDurableKnowledgeFailureIsJournaledAsAnEmptyBlock(t *testing.T) {
	provider := &stubKnowledgeContext{err: errors.New("store busy")}
	executor, _ := newKnowledgeExecutor(t, &capturingModel{answer: "done", tokens: 1}, provider)
	inv := &journalInvocation{}
	req := knowledgeTurnRequest("support")
	first, _ := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
	provider.mu.Lock()
	provider.err, provider.evidence = nil, turn.KnowledgeEvidence{Block: knowledgeBlockFixture, ChunkIDs: []string{"kbc_1"}}
	provider.mu.Unlock()
	replayed, _ := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)

	if first.Block != "" || first.Outcome != knowledgeOutcomeError || replayed.Block != "" || replayed.Outcome != knowledgeOutcomeError {
		t.Fatalf("first = %+v replayed = %+v, want the failure journaled and never re-queried", first, replayed)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1", provider.calls)
	}
	if _, journaled := inv.journal[knowledgeAction]; !journaled {
		t.Error("the failure was not journaled")
	}
}

func TestDurableKnowledgeSkipsTheActionForDescriptorsWithoutAList(t *testing.T) {
	provider := okKnowledge()
	executor, _ := newKnowledgeExecutor(t, &capturingModel{answer: "done", tokens: 1}, provider)
	inv := &journalInvocation{}
	journal, ran := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), knowledgeTurnRequest())
	if ran || journal.Block != "" || len(inv.journal) != 0 || provider.calls != 0 {
		t.Fatalf("ran = %t journal = %v calls = %d, want the action skipped so in-flight turns replay unchanged", ran, inv.journal, provider.calls)
	}
}

func TestKnowledgeJournalRejectsUnknownFieldsAndFailsClosed(t *testing.T) {
	fallback := knowledgeJournal{ChunkIDs: []string{}, Outcome: knowledgeOutcomeError}
	for name, raw := range map[string]string{
		"unknown field": `{"block":"x","outcome":"ok","extra":1}`,
		"not json":      `block`,
		"trailing":      `{"outcome":"ok"} {"outcome":"ok"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := decodeKnowledgeJournal([]byte(raw), &fallback)
			if got.Block != "" || got.Outcome != knowledgeOutcomeError {
				if name != "trailing" {
					t.Errorf("decoded = %+v, want the fail-closed fallback", got)
				}
			}
		})
	}
	good := decodeKnowledgeJournal([]byte(`{"block":"b","outcome":"ok"}`), &fallback)
	if good.Block != "b" || good.ChunkIDs == nil {
		t.Errorf("good = %+v", good)
	}
}

func TestWithKnowledgeContextRejectsNil(t *testing.T) {
	if err := WithKnowledgeContext(nil)(&ADKExecutor{}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestToolsForOffersKnowledgeSearchOnlyWithAStampedList(t *testing.T) {
	executor := &ADKExecutor{
		tools: []tool.Tool{namedToolStub{name: "read_file"}, namedToolStub{name: auratools.KBSearchToolName}},
	}
	names := func(tools []tool.Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}
		return out
	}
	listed := &auraagent.Definition{
		Audience: "public", Tools: []string{"read_file", auratools.KBSearchToolName},
		PrincipalTools: []string{"read_file", auratools.KBSearchToolName}, PublicTools: []string{auratools.KBSearchToolName},
	}
	explicitWithoutSearch := &auraagent.Definition{Tools: []string{"read_file"}, PrincipalTools: []string{"read_file"}, PublicTools: []string{}}
	scope := []string{"support"}
	cases := map[string]struct {
		definition *auraagent.Definition
		class      principal.Class
		knowledge  []string
		want       []string
	}{
		"owner with knowledge, implicit tools":     {&auraagent.Definition{}, principal.ClassOwner, scope, []string{"read_file", "kb_search"}},
		"owner without knowledge":                  {&auraagent.Definition{}, principal.ClassOwner, nil, []string{"read_file"}},
		"owner explicit list bypassed":             {explicitWithoutSearch, principal.ClassOwner, scope, []string{"read_file", "kb_search"}},
		"owner explicit list without knowledge":    {explicitWithoutSearch, principal.ClassOwner, nil, []string{"read_file"}},
		"trusted listed":                           {listed, principal.ClassTrusted, scope, []string{"read_file", "kb_search"}},
		"trusted listed but no knowledge":          {listed, principal.ClassTrusted, nil, []string{"read_file"}},
		"trusted not listed":                       {explicitWithoutSearch, principal.ClassTrusted, scope, []string{"read_file"}},
		"public listed":                            {listed, principal.ClassPublic, scope, []string{"kb_search"}},
		"public not listed":                        {explicitWithoutSearch, principal.ClassPublic, scope, nil},
		"unclassified without knowledge":           {listed, "", nil, []string{"read_file"}},
		"persona without knowledge, implicit list": {&auraagent.Definition{}, principal.ClassOwner, nil, []string{"read_file"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := names(executor.toolsFor(tc.definition, tc.class, "", tc.knowledge))
			if len(got) != len(tc.want) || (len(got) > 0 && !slices.Equal(got, tc.want)) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuiltinToolRequestCarriesTheStampedKnowledgeList(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName, models := fakeModelSet(model)
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name: "sample_tool", Version: "v1", Description: "sample",
			Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		}},
		output: json.RawMessage(`{"ok":true}`),
	}
	db, sessions, _ := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	req := &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", PrincipalClass: principal.ClassOwner,
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hi"}}, Knowledge: []string{"faq", "support"},
	}
	for _, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if len(builtin.requests) != 1 || !slices.Equal(builtin.requests[0].Knowledge, []string{"faq", "support"}) || !builtin.requests[0].Tainted {
		t.Fatalf("builtin requests = %+v, want the stamped list and the taint flag on the tool call", builtin.requests)
	}
}

func TestBuiltinToolRequestIsUntaintedWithoutAStampedList(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName, models := fakeModelSet(model)
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name: "sample_tool", Version: "v1", Description: "sample",
			Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		}},
		output: json.RawMessage(`{"ok":true}`),
	}
	db, sessions, _ := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	req := &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", PrincipalClass: principal.ClassOwner,
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hi"}},
	}
	for _, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if len(builtin.requests) != 1 || builtin.requests[0].Tainted || len(builtin.requests[0].Knowledge) != 0 {
		t.Fatalf("builtin requests = %+v, want an untainted call", builtin.requests)
	}
}

func TestKnowledgeModeIsReportedInTheEventAndSurvivesAReplay(t *testing.T) {
	for _, mode := range []string{"lexical", "hybrid", "lexical_fallback"} {
		t.Run(mode, func(t *testing.T) {
			provider := okKnowledge()
			provider.evidence.Mode = mode
			executor, _ := newKnowledgeExecutor(t, &capturingModel{answer: "done", tokens: 1}, provider)
			events := drainTurn(t, executor, knowledgeTurnRequest("support"))
			found := retrievedEvents(events)
			if len(found) != 1 {
				t.Fatalf("retrieved events = %d", len(found))
			}
			if got := decodeRetrieved(t, &found[0]); got.Mode != mode || got.Outcome != knowledgeOutcomeOK {
				t.Errorf("event mode %q outcome %q, want %q and ok", got.Mode, got.Outcome, mode)
			}

			inv := &journalInvocation{}
			req := knowledgeTurnRequest("support")
			first, _ := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
			provider.mu.Lock()
			provider.evidence.Mode = "changed"
			provider.mu.Unlock()
			replayed, _ := executor.knowledgeContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
			if first.Mode != mode || replayed.Mode != mode {
				t.Errorf("first mode %q, replayed mode %q, want the journaled %q", first.Mode, replayed.Mode, mode)
			}
		})
	}
}

func TestKnowledgeModeIsOmittedWhenRetrievalFailed(t *testing.T) {
	provider := &stubKnowledgeContext{err: errors.New("store busy")}
	executor, _ := newKnowledgeExecutor(t, &capturingModel{answer: "done", tokens: 1}, provider)
	events := drainTurn(t, executor, knowledgeTurnRequest("support"))
	found := retrievedEvents(events)
	if len(found) != 1 {
		t.Fatalf("retrieved events = %d", len(found))
	}
	if got := decodeRetrieved(t, &found[0]); got.Mode != "" || got.Outcome != knowledgeOutcomeError {
		t.Errorf("payload = %+v, want no mode on a failed retrieval", got)
	}
}

func TestKnowledgeJournalWrittenBeforeModeExistedStillDecodes(t *testing.T) {
	fallback := knowledgeJournal{ChunkIDs: []string{}, Outcome: knowledgeOutcomeError}
	got := decodeKnowledgeJournal([]byte(`{"block":"b","chunk_ids":["c"],"counts":{"retrieved":1,"rendered":1},"duration_ms":3,"outcome":"ok"}`), &fallback)
	if got.Block != "b" || got.Outcome != knowledgeOutcomeOK || got.Mode != "" {
		t.Errorf("decoded = %+v", got)
	}
}
