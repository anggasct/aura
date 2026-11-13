package runtimeadk

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

const (
	durableTurnPoll    = 10 * time.Millisecond
	durableTurnHostTTL = time.Second
	durableTurnWait    = 20 * time.Second
	durableTurnText    = "find the answer"
)

type requestPart struct {
	role         string
	text         string
	callID       string
	callName     string
	responseID   string
	responseName string
	output       string
}

type requestParts []requestPart

func (r requestParts) count(match func(requestPart) bool) int {
	n := 0
	for _, part := range r {
		if match(part) {
			n++
		}
	}
	return n
}

func isUserText(part requestPart) bool { return part.text == durableTurnText }
func isCall(part requestPart) bool     { return part.callName != "" }
func isResponse(part requestPart) bool { return part.responseName != "" }
func hasPartID(part requestPart) bool  { return part.callID != "" || part.responseID != "" }

type toolThenTextModel struct {
	callID  string
	queries []string

	mu       sync.Mutex
	requests []requestParts
}

func (m *toolThenTextModel) Name() string { return "tool-then-text-model" }

func (m *toolThenTextModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var seen requestParts
		for _, content := range req.Contents {
			for _, part := range content.Parts {
				captured := requestPart{role: content.Role, text: part.Text}
				if part.FunctionCall != nil {
					captured.callID, captured.callName = part.FunctionCall.ID, part.FunctionCall.Name
				}
				if part.FunctionResponse != nil {
					captured.responseID, captured.responseName = part.FunctionResponse.ID, part.FunctionResponse.Name
					output, _ := json.Marshal(part.FunctionResponse.Response)
					captured.output = string(output)
				}
				seen = append(seen, captured)
			}
		}
		m.mu.Lock()
		m.requests = append(m.requests, seen)
		call := len(m.requests)
		m.mu.Unlock()
		parts := []*genai.Part{{Text: "done"}}
		if call == 1 && len(m.queries) > 0 {
			parts = nil
			for _, query := range m.queries {
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{Name: "tool-a", Args: map[string]any{"query": query}}})
			}
		} else if call == 1 {
			parts = []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: m.callID, Name: "tool-a", Args: map[string]any{"query": "x"}}}}
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			},
		}, nil)
	}
}

func (m *toolThenTextModel) seen() []requestParts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]requestParts(nil), m.requests...)
}

type gatedBrokerTools struct {
	broker  *toolbroker.Broker
	release chan struct{}

	mu         sync.Mutex
	keys       []string
	executions int
	runs       map[string]int
	stalled    string
}

func (g *gatedBrokerTools) Definitions() []BuiltinToolDefinition {
	return []BuiltinToolDefinition{{Name: "tool-a", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`)}}
}

func (g *gatedBrokerTools) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return g.broker.Evaluate(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, RequestDigest: request.RequestDigest, Capabilities: request.Capabilities,
		Trust: request.Trust, Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
	})
}

func (g *gatedBrokerTools) Execute(ctx context.Context, request *BuiltinToolRequest) (json.RawMessage, error) {
	g.mu.Lock()
	g.keys = append(g.keys, request.IdempotencyKey)
	g.mu.Unlock()
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result, err := g.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID:      request.RequestID,
		TurnID:         request.TurnID,
		SessionID:      request.SessionID,
		PrincipalID:    request.PrincipalID,
		ToolName:       request.ToolName,
		ToolVersion:    request.ToolVersion,
		Arguments:      request.Arguments,
		Capabilities:   request.Capabilities,
		Trust:          approval.TrustLabel(request.Trust),
		Deadline:       request.Deadline,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

func (g *gatedBrokerTools) execute(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	var arguments struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return toolbroker.ToolResult{}, err
	}
	g.mu.Lock()
	g.executions++
	g.runs[arguments.Query]++
	stall := arguments.Query == g.stalled && g.runs[arguments.Query] == 1
	g.mu.Unlock()
	if stall {
		<-ctx.Done()
		return toolbroker.ToolResult{}, ctx.Err()
	}
	output, err := json.Marshal(map[string]string{"query": arguments.Query})
	return toolbroker.ToolResult{Output: output}, err
}

func (g *gatedBrokerTools) runsOf(query string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs[query]
}

func (g *gatedBrokerTools) seenKeys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.keys...)
}

func (g *gatedBrokerTools) executionCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.executions
}

type durableTurnRig struct {
	db       *sql.DB
	engine   *runtimeengine.Engine
	model    *toolThenTextModel
	tools    *gatedBrokerTools
	sessions store.SessionService
	events   store.EventStore
	stopHost func()
}

func startDurableTurnRig(t *testing.T, model *toolThenTextModel) *durableTurnRig {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	mustCreateSession(t, db, "session-1")
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	gated := &gatedBrokerTools{broker: broker, release: make(chan struct{}), runs: map[string]int{}}
	if err := broker.RegisterTool(
		&tools.Definition{Name: "tool-a", Version: "v1", Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }},
		gated.execute,
		&approval.Rule{ToolName: "tool-a", ToolVersion: "v1"},
	); err != nil {
		t.Fatalf("register tool-a: %v", err)
	}
	rig := &durableTurnRig{db: db, model: model, tools: gated, sessions: sessions, events: events}
	rig.startLife(t)
	return rig
}

func (r *durableTurnRig) startLife(t *testing.T) {
	t.Helper()
	executor, err := NewADKExecutor("aura", r.model.Name(), auramodel.Set{r.model.Name(): r.model}, r.sessions, r.events, r.tools, nil, nil, WithBuiltinToolExecutor(r.tools))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	rt, err := sqlite.New(&sqlite.Options{DB: r.db, Logger: slog.New(slog.DiscardHandler), PollInterval: durableTurnPoll, HostTTL: durableTurnHostTTL})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	sessionStore, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: r.db})
	if err != nil {
		t.Fatalf("sqlite.NewSessionStore: %v", err)
	}
	eng, err := runtimeengine.NewEngine(&runtimeengine.Config{
		MaxActiveTurns:  4,
		MaxPendingTurns: 16,
		TurnTimeout:     time.Minute,
		Durable:         &runtimeengine.DurableConfig{Sessions: sessionStore, Runtime: rt},
	}, r.events, store.NewDedupeStore(r.db), executor, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	executor.SetEventPublisher(eng)
	hostCtx, cancelHost := context.WithCancel(context.WithoutCancel(t.Context()))
	hostDone := make(chan error, 1)
	go func() { hostDone <- rt.Run(hostCtx) }()
	stopHost := sync.OnceFunc(func() {
		cancelHost()
		select {
		case <-hostDone:
		case <-time.After(durableTurnWait):
			t.Error("durable host did not stop")
		}
	})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = eng.Shutdown(shutdownCtx)
		stopHost()
	})
	if r.engine == nil {
		r.engine = eng
	}
	r.stopHost = stopHost
}

func (r *durableTurnRig) run(ctx context.Context) <-chan error {
	out := make(chan error, 1)
	go func() {
		var streamErr error
		for _, err := range r.engine.Run(ctx, &turn.TurnRequest{
			TurnID:         "turn-1",
			SessionID:      "session-1",
			PrincipalID:    "user-1",
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          []runtimeingress.InputPart{{Text: durableTurnText}},
		}) {
			if err != nil {
				streamErr = err
				break
			}
		}
		out <- streamErr
	}()
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(durableTurnWait)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func awaitStream(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("turn stream: %v", err)
		}
	case <-time.After(durableTurnWait):
		t.Fatal("the turn stream never ended")
	}
}

func (r *durableTurnRig) runClaim(t *testing.T) int {
	t.Helper()
	var claim int
	if err := r.db.QueryRowContext(t.Context(), `SELECT claim FROM durable_run WHERE key = ?`, "turn-1").Scan(&claim); err != nil {
		t.Fatalf("read the claim of turn-1: %v", err)
	}
	return claim
}

func (r *durableTurnRig) turnEvents(t *testing.T) []store.RuntimeEvent {
	t.Helper()
	events, err := store.NewDedupeStore(r.db).ListTurnEvents(t.Context(), "session-1", "turn-1")
	if err != nil {
		t.Fatalf("list the events of turn-1: %v", err)
	}
	return events
}

func requireEachMessageOnce(t *testing.T, request requestParts) {
	t.Helper()
	if n := request.count(isUserText); n != 1 {
		t.Errorf("user text in the request = %d, want 1 (request %+v)", n, request)
	}
	if n := request.count(isCall); n != 1 {
		t.Errorf("function calls in the request = %d, want 1 (request %+v)", n, request)
	}
	if n := request.count(isResponse); n != 1 {
		t.Errorf("function responses in the request = %d, want 1 (request %+v)", n, request)
	}
}

func TestDurableTurnGivesTheModelEachMessageOnce(t *testing.T) {
	rig := startDurableTurnRig(t, &toolThenTextModel{callID: "call-1"})
	close(rig.tools.release)

	awaitStream(t, rig.run(context.WithoutCancel(t.Context())))

	requests := rig.model.seen()
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	requireEachMessageOnce(t, requests[1])
}

func TestReplayedTurnGivesTheModelEachMessageOnce(t *testing.T) {
	rig := startDurableTurnRig(t, &toolThenTextModel{})
	done := rig.run(context.WithoutCancel(t.Context()))
	waitFor(t, "the first invocation reaches tool-a", func() bool { return len(rig.tools.seenKeys()) == 1 })

	if _, err := rig.db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
		t.Fatalf("delete the host lease: %v", err)
	}
	waitFor(t, "the run of turn-1 is claimed again", func() bool { return rig.runClaim(t) >= 2 })
	close(rig.tools.release)
	awaitStream(t, done)

	events := rig.turnEvents(t)
	var terminals []store.RuntimeEvent
	var adk []store.RuntimeEvent
	for i := range events {
		switch events[i].Kind {
		case turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled:
			terminals = append(terminals, events[i])
		case store.EventKindADK:
			adk = append(adk, events[i])
		}
	}
	if len(terminals) != 1 || terminals[0].Kind != turn.EventKindTurnCompleted || terminals[0].ID != "turn-1-terminal" {
		t.Fatalf("terminal events = %+v, want one turn.completed with the id turn-1-terminal", terminals)
	}
	var stored requestParts
	for i := range adk {
		if want := fmt.Sprintf("turn-1-ev-%d", i+1); adk[i].ID != want {
			t.Errorf("stored ADK event %d id = %q, want %q", i+1, adk[i].ID, want)
		}
		ev, err := store.RuntimeEventToADK(&adk[i])
		if err != nil {
			t.Fatalf("decode stored ADK event %s: %v", adk[i].ID, err)
		}
		if ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			captured := requestPart{role: ev.Content.Role, text: part.Text}
			if part.FunctionCall != nil {
				captured.callID, captured.callName = part.FunctionCall.ID, part.FunctionCall.Name
			}
			if part.FunctionResponse != nil {
				captured.responseID, captured.responseName = part.FunctionResponse.ID, part.FunctionResponse.Name
			}
			stored = append(stored, captured)
		}
	}
	requireEachMessageOnce(t, stored)
	var callID, responseID string
	for _, part := range stored {
		if isCall(part) {
			callID = part.callID
		}
		if isResponse(part) {
			responseID = part.responseID
		}
	}
	if callID == "" || responseID != callID {
		t.Errorf("stored function call id = %q, response id = %q, want one non-empty id on both", callID, responseID)
	}

	requests := rig.model.seen()
	if len(requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(requests))
	}
	afterReplay := requests[1]
	requireEachMessageOnce(t, afterReplay)
	for _, part := range afterReplay {
		if (isCall(part) || isResponse(part)) && hasPartID(part) {
			t.Errorf("request part %+v carries an id, want the stamped id removed", part)
		}
	}
	keys := rig.tools.seenKeys()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Errorf("tool-a idempotency keys = %v, want the same key in both invocations", keys)
	}
	if n := rig.tools.executionCount(); n != 1 {
		t.Errorf("tool-a executions = %d, want 1", n)
	}
}
