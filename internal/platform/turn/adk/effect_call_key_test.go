package runtimeadk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/effect"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type scriptedModel struct {
	responses [][]*genai.Part

	mu       sync.Mutex
	calls    int
	requests []requestParts
}

func (m *scriptedModel) Name() string { return "scripted-model" }

func (m *scriptedModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		seen := capturedParts(req)
		m.mu.Lock()
		call := m.calls
		m.calls++
		m.requests = append(m.requests, seen)
		m.mu.Unlock()
		if call >= len(m.responses) {
			yield(nil, errors.New("the scripted model has no response left"))
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: m.responses[call]},
			TurnComplete: true,
		}, nil)
	}
}

func (m *scriptedModel) seen() []requestParts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

func queryCall(id, query string) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: "tool-a", Args: map[string]any{"query": query}}}
}

func effectCall(id, note string) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: "tool-e", Args: map[string]any{"note": note}}}
}

func textResponse() []*genai.Part {
	return []*genai.Part{{Text: "done"}}
}

type effectTools struct {
	broker *toolbroker.Broker
	db     *sql.DB

	mu         sync.Mutex
	keys       []string
	requestIDs []string
	failures   []error
	runs       map[string]int
	blockFirst bool
}

func newEffectTools(t *testing.T, db *sql.DB) *effectTools {
	t.Helper()
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("effect.NewJournal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("effect.NewExecutor: %v", err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t), Effects: executor})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	e := &effectTools{broker: broker, db: db, runs: map[string]int{}}
	if err := broker.RegisterTool(
		&tools.Definition{Name: "tool-e", Version: "v1", Effectful: true, Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }},
		e.perform,
		&approval.Rule{ToolName: "tool-e", ToolVersion: "v1"},
	); err != nil {
		t.Fatalf("register tool-e: %v", err)
	}
	return e
}

func (e *effectTools) Definitions() []BuiltinToolDefinition {
	return []BuiltinToolDefinition{{Name: "tool-e", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`)}}
}

func (e *effectTools) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return evaluateThrough(ctx, e.broker, request)
}

func (e *effectTools) Execute(ctx context.Context, request *BuiltinToolRequest) (json.RawMessage, error) {
	e.mu.Lock()
	e.keys = append(e.keys, request.IdempotencyKey)
	e.requestIDs = append(e.requestIDs, request.RequestID)
	e.mu.Unlock()
	result, err := e.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID:             request.RequestID,
		TurnID:                request.TurnID,
		SessionID:             request.SessionID,
		PrincipalID:           request.PrincipalID,
		PrincipalClass:        request.PrincipalClass,
		ToolName:              request.ToolName,
		ToolVersion:           request.ToolVersion,
		Arguments:             request.Arguments,
		Capabilities:          request.Capabilities,
		Trust:                 approval.TrustLabel(request.Trust),
		Deadline:              request.Deadline,
		IdempotencyKey:        request.IdempotencyKey,
		AllocateEventSequence: true,
		EventInvocation:       request.EventInvocation,
		EventBranch:           request.EventBranch,
		EventAuthor:           request.EventAuthor,
	})
	if err != nil {
		e.mu.Lock()
		e.failures = append(e.failures, err)
		e.mu.Unlock()
		return nil, err
	}
	return result.Output, nil
}

func (e *effectTools) perform(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	var arguments struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return toolbroker.ToolResult{}, err
	}
	e.mu.Lock()
	e.runs[arguments.Note]++
	stall := e.blockFirst && e.totalRuns() == 1
	e.mu.Unlock()
	if stall {
		<-ctx.Done()
		return toolbroker.ToolResult{}, ctx.Err()
	}
	output, err := json.Marshal(map[string]string{"note": arguments.Note})
	return toolbroker.ToolResult{Output: output}, err
}

func (e *effectTools) totalRuns() int {
	total := 0
	for _, n := range e.runs {
		total += n
	}
	return total
}

func (e *effectTools) adapterRuns() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.totalRuns()
}

func (e *effectTools) seenKeys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.keys)
}

func (e *effectTools) seenRequestIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.requestIDs)
}

func (e *effectTools) seenFailures() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.failures)
}

type effectIntentRow struct {
	toolCallID string
	key        string
	state      string
}

func (e *effectTools) intents(t *testing.T) []effectIntentRow {
	t.Helper()
	rows, err := e.db.QueryContext(t.Context(), `SELECT tool_call_id, idempotency_key, state FROM effect_intent WHERE session_id = ? ORDER BY idempotency_key`, "session-1")
	if err != nil {
		t.Fatalf("read the effect intents of session-1: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var intents []effectIntentRow
	for rows.Next() {
		var row effectIntentRow
		if err := rows.Scan(&row.toolCallID, &row.key, &row.state); err != nil {
			t.Fatalf("scan an effect intent of session-1: %v", err)
		}
		intents = append(intents, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the effect intents of session-1: %v", err)
	}
	return intents
}

func effectTurn() *turn.TurnRequest {
	return ownerTurn("turn-1")
}

func ownerTurn(turnID string) *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID:         turnID,
		SessionID:      "session-1",
		PrincipalID:    "user-1",
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: durableTurnText}},
	}
}

func requireEachCallPerformed(t *testing.T, e *effectTools, wantKeys []string) {
	t.Helper()
	if got := e.adapterRuns(); got != len(wantKeys) {
		t.Errorf("adapter runs = %d, want %d", got, len(wantKeys))
	}
	if failures := e.seenFailures(); len(failures) != 0 {
		t.Errorf("broker errors = %v, want none", failures)
	}
	intents := e.intents(t)
	seenIDs := map[string]bool{}
	var got []effectIntentRow
	for _, intent := range intents {
		requireAuraCallID(t, intent.toolCallID)
		if seenIDs[intent.toolCallID] {
			t.Errorf("effect intents = %+v, want pairwise distinct tool call ids", intents)
		}
		seenIDs[intent.toolCallID] = true
		got = append(got, effectIntentRow{key: intent.key, state: intent.state})
	}
	var want []effectIntentRow
	for _, key := range wantKeys {
		want = append(want, effectIntentRow{key: key, state: "succeeded"})
	}
	if !slices.Equal(got, want) {
		t.Errorf("effect intents = %+v, want %+v", got, want)
	}
}

func TestRepeatedProviderCallIDKeepsEachEffect(t *testing.T) {
	cases := []struct {
		name      string
		responses [][]*genai.Part
		wantKeys  []string
	}{
		{
			name:      "the same id in two responses, the same arguments",
			responses: [][]*genai.Part{{effectCall("call_0", "one")}, {effectCall("call_0", "one")}, textResponse()},
			wantKeys:  []string{"adk/session-1/turn-1/call-1-0", "adk/session-1/turn-1/call-2-0"},
		},
		{
			name:      "the same id in two responses, other arguments",
			responses: [][]*genai.Part{{effectCall("call_0", "one")}, {effectCall("call_0", "two")}, textResponse()},
			wantKeys:  []string{"adk/session-1/turn-1/call-1-0", "adk/session-1/turn-1/call-2-0"},
		},
		{
			name:      "the same id twice in one response",
			responses: [][]*genai.Part{{effectCall("call_0", "one"), effectCall("call_0", "two")}, textResponse()},
			wantKeys:  []string{"adk/session-1/turn-1/call-1-0", "adk/session-1/turn-1/call-1-1"},
		},
	}
	t.Run("in memory", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				model := &scriptedModel{responses: tc.responses}
				db, sessions, _ := newSessionTestDB(t)
				mustCreateSession(t, db, "session-1")
				effects := newEffectTools(t, db)
				executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, effects, nil, nil, WithBuiltinToolExecutor(effects))
				if err != nil {
					t.Fatalf("NewADKExecutor: %v", err)
				}

				if err := runTurn(t.Context(), executor, effectTurn()); err != nil {
					t.Fatalf("Execute: %v", err)
				}

				requireEachCallPerformed(t, effects, tc.wantKeys)
			})
		}
	})
	t.Run("durable", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rig := newDurableTurnRig(t, &scriptedModel{responses: tc.responses})
				effects := newEffectTools(t, rig.db)
				rig.tools = effects
				rig.startLife(t)

				awaitStream(t, rig.run(context.WithoutCancel(t.Context())))

				requireEachCallPerformed(t, effects, tc.wantKeys)
			})
		}
	})
}

func TestReplayedEffectfulCallKeepsItsKey(t *testing.T) {
	rig := newDurableTurnRig(t, &scriptedModel{responses: [][]*genai.Part{{effectCall("call_0", "one")}, textResponse()}})
	effects := newEffectTools(t, rig.db)
	effects.blockFirst = true
	rig.tools = effects
	rig.startLife(t)
	rig.run(context.WithoutCancel(t.Context()))
	waitFor(t, "the first life is inside the effect", func() bool { return effects.adapterRuns() == 1 })

	rig.stopHost()
	rig.startLife(t)
	waitFor(t, "the second life ends the turn", func() bool { return rig.turnEnded(t) })

	if got := effects.adapterRuns(); got != 1 {
		t.Errorf("adapter runs over both lives = %d, want 1", got)
	}
	const wantKey = "adk/session-1/turn-1/call-1-0"
	intents := effects.intents(t)
	if len(intents) != 1 || intents[0].key != wantKey || intents[0].state == "prepared" || intents[0].state == "succeeded" {
		t.Errorf("effect intents = %+v, want one under %s that is neither prepared nor succeeded", intents, wantKey)
	}
	if got := effects.seenKeys(); !slices.Equal(got, []string{wantKey, wantKey}) {
		t.Errorf("keys received = %q, want %q once in each life", got, wantKey)
	}
}

type slotlessContext struct {
	agent.Context
}

func (slotlessContext) Value(any) any { return nil }

func TestBuiltinToolCallWithoutACallKeyIsRefused(t *testing.T) {
	_, sessions, _ := newSessionTestDB(t)
	model := &scriptedModel{}
	effects := &effectTools{runs: map[string]int{}}
	executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, effects, nil, nil, WithBuiltinToolExecutor(effects))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}

	_, err = executor.executeBuiltinTool(slotlessContext{}, "tool-e", map[string]any{"note": "one"})

	if code, ok := turn.CodeOf(err); !ok || code != turn.ErrorCodeInvalidArgument {
		t.Errorf("executeBuiltinTool error = %v, want %s", err, turn.ErrorCodeInvalidArgument)
	}
	var coded *turn.Error
	if !errors.As(err, &coded) || coded.Detail != `tool "tool-e" has no call key` {
		t.Errorf("executeBuiltinTool error = %v, want the missing call key", err)
	}
	if keys := effects.seenKeys(); len(keys) != 0 {
		t.Errorf("builtin executor calls = %q, want none", keys)
	}
}
