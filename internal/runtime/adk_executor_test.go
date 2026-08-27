package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// fakeADKModel returns a fixed answer with the given usage, optionally
// requesting a tool call.
type fakeADKModel struct {
	answer    string
	toolCall  bool
	tokens    int32
	callCount int
}

func (f *fakeADKModel) Name() string { return "fake-model" }

func (f *fakeADKModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		f.callCount++
		parts := []*genai.Part{{Text: f.answer}}
		// Request the tool on the first call only; the answer afterwards, so
		// the runner terminates instead of looping on the tool call.
		if f.toolCall && f.callCount == 1 {
			parts = []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call-1",
					Name: "sample_tool",
					Args: map[string]any{"query": "x"},
				},
			}}
		}
		if !yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     f.tokens,
				CandidatesTokenCount: f.tokens,
				TotalTokenCount:      f.tokens * 2,
			},
		}, nil) {
			return
		}
	}
}

// newFakeTool builds a proper ADK function tool so the runner invokes it
// through the declared tool set.
func newFakeTool(t *testing.T) tool.Tool {
	t.Helper()
	ft, _ := newRecordingTool(t)
	return ft
}

// newRecordingTool builds a proper ADK function tool and returns a flag that
// records whether the handler ran, so tests can assert fail-closed behavior.
func newRecordingTool(t *testing.T) (ft tool.Tool, executed *bool) {
	t.Helper()
	executed = new(bool)
	ft, err := functiontool.New[map[string]any, map[string]any](
		functiontool.Config{Name: "sample_tool", Description: "sample"},
		func(ctx agent.Context, args map[string]any) (map[string]any, error) {
			*executed = true
			return map[string]any{"ok": true}, nil
		},
	)
	if err != nil {
		t.Fatalf("newRecordingTool: %v", err)
	}
	return ft, executed
}

func registerFakeModel(t *testing.T, model *fakeADKModel) string {
	t.Helper()
	name := "fake-model-" + newTurnID()
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) {
		return model, nil
	})
	return name
}

type fakeBroker struct {
	deny     bool
	checked  []string
	requests []approval.ToolRequest
}

type fakeBuiltinExecutor struct {
	definitions []BuiltinToolDefinition
	requests    []*BuiltinToolRequest
	events      store.EventStore
	output      json.RawMessage
	err         error
	publish     func(*store.RuntimeEvent)
	// block, when non-nil, holds Execute open after the tool request is
	// published, modeling a provider that is still running.
	block chan struct{}
}

func (f *fakeBuiltinExecutor) SetEventPublisher(publish func(*store.RuntimeEvent)) {
	f.publish = publish
}

func (f *fakeBuiltinExecutor) Definitions() []BuiltinToolDefinition {
	return cloneBuiltinDefinitions(f.definitions)
}

func (f *fakeBuiltinExecutor) Execute(ctx context.Context, request *BuiltinToolRequest) (json.RawMessage, error) {
	f.requests = append(f.requests, request)
	if f.events != nil {
		payload, err := json.Marshal(map[string]string{"operation": request.ToolName})
		if err != nil {
			return nil, err
		}
		event := &store.RuntimeEvent{
			ID: request.RequestID + "-requested", SessionID: request.SessionID, Sequence: request.EventSequence,
			TurnID: request.TurnID, InvocationID: request.EventInvocation, Author: request.EventAuthor,
			Kind: EventKindToolRequested, SchemaVersion: 1, Payload: payload, CreatedAt: time.Now().UTC(),
		}
		if err := f.events.Append(ctx, event); err != nil {
			return nil, err
		}
		if f.publish != nil {
			f.publish(event)
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.output, nil
}

type recordingPublisher struct {
	events []store.RuntimeEvent
}

func (p *recordingPublisher) Publish(event *store.RuntimeEvent) {
	p.events = append(p.events, *event)
}

func (b *fakeBroker) Evaluate(ctx context.Context, req *approval.ToolRequest) (approval.PolicyDecision, error) {
	b.checked = append(b.checked, req.ToolName)
	b.requests = append(b.requests, *req)
	if b.deny {
		return approval.PolicyDecision{Outcome: "deny"}, nil
	}
	return approval.PolicyDecision{Outcome: "allow"}, nil
}

func newADKTestExecutor(t *testing.T, modelName string, broker ToolBroker, tools ...tool.Tool) (*ADKExecutor, *sql.DB, store.EventStore) {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, broker, tools, nil)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	return executor, db, events
}

func TestADKExecutorRunsTurnAndPersists(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 5}
	modelName := registerFakeModel(t, model)
	broker := &fakeBroker{}
	executor, db, _ := newADKTestExecutor(t, modelName, broker)
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{
		TurnID:      "turn-1",
		SessionID:   "session-1",
		PrincipalID: "user-1",
		Origin:      OriginTerminal,
		Parts:       []InputPart{{Text: "hello"}},
	}
	var events []store.RuntimeEvent
	for ev, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("no events produced")
	}
	var final string
	for _, ev := range events {
		if ev.Kind == store.EventKindADK && ev.TurnID == "turn-1" {
			final = string(ev.Payload)
		}
	}
	if !strings.Contains(final, "final answer") {
		t.Errorf("payload = %s, want the model answer", final)
	}
	// The runner must have loaded the session through the adapter.
	if len(broker.checked) != 0 {
		t.Errorf("unexpected tool evaluations: %v", broker.checked)
	}
}

func TestADKExecutorGatesToolCalls(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName := registerFakeModel(t, model)
	broker := &fakeBroker{}
	executor, db, _ := newADKTestExecutor(t, modelName, broker, newFakeTool(t))
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal, Parts: []InputPart{{Text: "hi"}}}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Logf("executor error: %v", err)
		}
	}
	if len(broker.checked) == 0 {
		t.Fatal("tool call was not evaluated by the broker")
	}
	// The broker must receive the full identity the security contract
	// requires, bound from the invocation context.
	got := broker.requests[len(broker.requests)-1]
	if got.PrincipalID != "user-1" {
		t.Errorf("PrincipalID = %q, want user-1", got.PrincipalID)
	}
	if got.SessionID != "session-1" {
		t.Errorf("SessionID = %q, want session-1", got.SessionID)
	}
	if got.TurnID == "" || got.RequestID == "" {
		t.Errorf("TurnID/RequestID empty: %+v", got)
	}
	if got.ToolName != "sample_tool" {
		t.Errorf("ToolName = %q, want sample_tool", got.ToolName)
	}
	if got.Trust != approval.TrustDerivedUntrusted {
		t.Errorf("Trust = %q, want derived_untrusted", got.Trust)
	}
}

func TestADKExecutorDeniedToolFailsClosed(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName := registerFakeModel(t, model)
	broker := &fakeBroker{deny: true}
	gateTool, executed := newRecordingTool(t)
	executor, db, _ := newADKTestExecutor(t, modelName, broker, gateTool)
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal, Parts: []InputPart{{Text: "hi"}}}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Logf("executor error: %v", err)
		}
	}
	if len(broker.checked) == 0 {
		t.Fatal("denied tool was not evaluated by the broker")
	}
	if *executed {
		t.Fatal("denied tool call was executed despite the denial")
	}
}

func TestADKExecutorRunsBuiltInThroughExecutor(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName := registerFakeModel(t, model)
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name:                 "sample_tool",
			Version:              "v1",
			Description:          "sample",
			Schema:               json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"sample-capability"},
		}},
		output: json.RawMessage(`{"ok":true}`),
	}
	db, sessions, events := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal, Parts: []InputPart{{Text: "hi"}}}
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if len(builtin.requests) != 1 {
		t.Fatalf("builtin calls = %d, want 1", len(builtin.requests))
	}
	got := builtin.requests[0]
	if got.ToolName != "sample_tool" || got.ToolVersion != "v1" || got.TurnID != "turn-1" || got.SessionID != "session-1" || got.PrincipalID != "user-1" {
		t.Fatalf("builtin request identity = %+v", got)
	}
	if got.EventSequence == 0 || got.RequestID == "" || got.IdempotencyKey == "" {
		t.Fatalf("builtin request lacks execution identity = %+v", got)
	}
	if got.Trust != "derived_untrusted" || len(got.Capabilities) != 1 || got.Capabilities[0] != "sample-capability" {
		t.Fatalf("builtin request policy fields = %+v", got)
	}
}

func TestADKExecutorPublishesDurableBuiltinEvents(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name: "sample_tool", Version: "v1", Description: "sample",
			Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}},
		events: events,
		output: json.RawMessage(`{"ok":true}`),
	}
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	publisher := &recordingPublisher{}
	executor.SetEventPublisher(publisher)
	mustCreateSession(t, db, "session-1")

	for _, err := range executor.Execute(context.Background(), &TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal,
		Parts: []InputPart{{Text: "hi"}},
	}) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if len(publisher.events) != 1 || publisher.events[0].Kind != EventKindToolRequested {
		t.Fatalf("published events = %+v, want one tool.requested event", publisher.events)
	}
}

func TestADKExecutorBudgetEnforced(t *testing.T) {
	model := &fakeADKModel{answer: "final answer", tokens: 50}
	modelName := registerFakeModel(t, model)
	executor, db, _ := newADKTestExecutor(t, modelName, &fakeBroker{})
	mustCreateSession(t, db, "session-1")

	req := &TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal,
		Parts:  []InputPart{{Text: "hi"}},
		Budget: Budget{MaxTokens: 10},
	}
	var lastErr error
	for _, err := range executor.Execute(context.Background(), req) {
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Fatal("budget exceeded but no error")
	}
	if code, ok := CodeOf(lastErr); !ok || code != ErrorCodeBudgetExhausted {
		t.Fatalf("CodeOf(%v) = %q, %v; want budget_exhausted", lastErr, code, ok)
	}
}

// TestBuiltinToolRequestStreamsWhileProviderRuns proves the live delivery
// path the interactive console depends on: the committed tool request reaches
// the runtime's live subscriber while the provider is still running, not
// after the tool settles.
func TestBuiltinToolRequestStreamsWhileProviderRuns(t *testing.T) {
	model := &fakeADKModel{answer: "done", toolCall: true, tokens: 3}
	modelName := registerFakeModel(t, model)
	db, sessions, events := newSessionTestDB(t)
	block := make(chan struct{})
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name: "sample_tool", Version: "v1", Description: "sample",
			Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}},
		events: events,
		output: json.RawMessage(`{"ok":true}`),
		block:  block,
	}
	executor, err := NewADKExecutor("aura", modelName, sessions, events, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	engine, err := NewEngine(Config{}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	executor.SetEventPublisher(engine)
	mustCreateSession(t, db, "session-1")

	kinds := make(chan string, 16)
	runDone := make(chan error, 1)
	go func() {
		var runErr error
		for ev, err := range engine.Run(context.Background(), &TurnRequest{
			TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: OriginTerminal,
			Parts: []InputPart{{Text: "hi"}},
		}) {
			if err != nil {
				runErr = err
				break
			}
			kinds <- ev.Kind
			if ev.Kind == EventKindTurnCompleted || ev.Kind == EventKindTurnFailed {
				break
			}
		}
		runDone <- runErr
	}()

	// The tool provider never releases during this window: the request must
	// still reach the live subscriber. Earlier turn events are expected; the
	// assertion is that tool.requested arrives before the release.
	deadline := time.After(2 * time.Second)
	sawToolRequested := false
	for !sawToolRequested {
		select {
		case kind := <-kinds:
			if kind == EventKindToolRequested {
				sawToolRequested = true
			}
		case <-deadline:
			t.Fatal("tool request did not stream while the provider was running")
		}
	}

	// Drain the remaining blocked work: release the provider and wait for a
	// terminal event so the run goroutine exits.
	close(block)
	deadline = time.After(2 * time.Second)
	for {
		select {
		case kind := <-kinds:
			if kind == EventKindTurnCompleted || kind == EventKindTurnFailed {
				if err := <-runDone; err != nil {
					t.Fatalf("run: %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("turn did not finish after the provider was released")
		}
	}
}
