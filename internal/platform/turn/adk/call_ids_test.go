package runtimeadk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/store"
)

var auraCallID = regexp.MustCompile(`^call_[a-z2-7]{26}$`)

func requireAuraCallID(t *testing.T, id string) {
	t.Helper()
	if !auraCallID.MatchString(id) {
		t.Errorf("call id = %q, want call_ and 26 characters of a-z2-7", id)
	}
}

type chunk struct {
	response *adkmodel.LLMResponse
	err      error
}

type chunkModel struct {
	scripts [][]chunk

	mu    sync.Mutex
	calls int
}

func (m *chunkModel) Name() string { return "chunk-model" }

func (m *chunkModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	script := m.scripts[m.calls]
	m.calls++
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for _, c := range script {
			if !yield(c.response, c.err) {
				return
			}
		}
	}
}

type yielded struct {
	response *adkmodel.LLMResponse
	err      error
}

func drain(model adkmodel.LLM) []yielded {
	var out []yielded
	for response, err := range model.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		out = append(out, yielded{response: response, err: err})
	}
	return out
}

func callIDsOf(response *adkmodel.LLMResponse) []string {
	var ids []string
	for _, part := range response.Content.Parts {
		if part.FunctionCall != nil {
			ids = append(ids, part.FunctionCall.ID)
		}
	}
	return ids
}

func toolCalls(ids ...string) *adkmodel.LLMResponse {
	parts := make([]*genai.Part, len(ids))
	for i, id := range ids {
		parts[i] = queryCall(id, fmt.Sprintf("q%d", i))
	}
	return &adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}, TurnComplete: true}
}

func TestEveryFunctionCallGetsItsOwnID(t *testing.T) {
	t.Run("names every call of a final response with an id of its own", func(t *testing.T) {
		partial := &adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "thinking"}}}, Partial: true}
		first := toolCalls("call_0", "call_0", "")
		first.Content.Parts = append(first.Content.Parts, &genai.Part{Text: "and a note"})
		text := &adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}, TurnComplete: true}
		second := toolCalls("call_0")
		model := &chunkModel{scripts: [][]chunk{
			{{response: partial}, {response: first}, {response: nil}, {response: text}},
			{{response: second}},
		}}
		named := nameCalls(model)

		firstRun := drain(named)
		secondRun := drain(named)

		if len(firstRun) != 4 || len(secondRun) != 1 {
			t.Fatalf("yielded %d and %d responses, want 4 and 1", len(firstRun), len(secondRun))
		}
		if firstRun[0].response != partial {
			t.Error("the partial response was replaced, want it yielded unchanged")
		}
		if firstRun[2].response != nil || firstRun[2].err != nil {
			t.Errorf("the nil response was yielded as %+v, want (nil, nil)", firstRun[2])
		}
		if firstRun[3].response != text {
			t.Error("the response without calls was replaced, want it yielded unchanged")
		}
		ids := append(callIDsOf(firstRun[1].response), callIDsOf(secondRun[0].response)...)
		if len(ids) != 4 {
			t.Fatalf("named %d calls, want 4", len(ids))
		}
		for _, id := range ids {
			requireAuraCallID(t, id)
		}
		if distinct := len(slices.Compact(slices.Sorted(slices.Values(ids)))); distinct != len(ids) {
			t.Errorf("ids = %v, want pairwise distinct", ids)
		}
		for i, part := range firstRun[1].response.Content.Parts[:3] {
			original := first.Content.Parts[i].FunctionCall
			if part.FunctionCall.Name != original.Name || !reflect.DeepEqual(part.FunctionCall.Args, original.Args) {
				t.Errorf("call %d = %+v, want the name and arguments of %+v", i, part.FunctionCall, original)
			}
		}
		if last := firstRun[1].response.Content.Parts[3]; last.Text != "and a note" {
			t.Errorf("the part after the calls = %+v, want it kept", last)
		}
		if got := callIDsOf(first); !slices.Equal(got, []string{"call_0", "call_0", ""}) {
			t.Errorf("the provider's response carries ids %v afterwards, want it untouched", got)
		}
		if got := callIDsOf(second); !slices.Equal(got, []string{"call_0"}) {
			t.Errorf("the second provider response carries ids %v afterwards, want it untouched", got)
		}
	})
	t.Run("yields an error unchanged", func(t *testing.T) {
		boom := errors.New("provider failed")
		model := &chunkModel{scripts: [][]chunk{{{err: boom}}}}

		got := drain(nameCalls(model))

		if len(got) != 1 || got[0].response != nil || !errors.Is(got[0].err, boom) {
			t.Errorf("yielded %+v, want the provider error alone", got)
		}
	})
	t.Run("stops when the consumer stops", func(t *testing.T) {
		partial := &adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "thinking"}}}, Partial: true}
		model := &chunkModel{scripts: [][]chunk{{{response: partial}, {response: toolCalls("call_0")}, {response: toolCalls("call_1")}}}}
		seen := 0

		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("the wrapper kept iterating after the consumer stopped: %v", recovered)
				}
			}()
			for range nameCalls(model).GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
				seen++
				break
			}
		}()

		if seen != 1 {
			t.Errorf("consumer saw %d responses, want 1", seen)
		}
	})
}

type memoryRig struct {
	db       *sql.DB
	sessions store.SessionService
	events   store.EventStore
	model    adkmodel.LLM
	broker   turn.ToolBroker
	builtins BuiltinToolExecutor
}

func newMemoryRig(t *testing.T, model adkmodel.LLM) *memoryRig {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	mustCreateSession(t, db, "session-1")
	return &memoryRig{db: db, sessions: sessions, events: events, model: model, broker: &fakeBroker{}}
}

func (r *memoryRig) engine(t *testing.T) *runtimeengine.Engine {
	t.Helper()
	executor, err := NewADKExecutor("aura", r.model.Name(), auramodel.Set{r.model.Name(): r.model}, r.sessions, r.broker, nil, nil, WithBuiltinToolExecutor(r.builtins))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	engine, err := runtimeengine.NewEngine(&runtimeengine.Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, r.events, store.NewDedupeStore(r.db), executor, nil)
	if err != nil {
		t.Fatalf("runtimeengine.NewEngine: %v", err)
	}
	return engine
}

func runEngineTurn(ctx context.Context, engine *runtimeengine.Engine, req *turn.TurnRequest) error {
	for _, err := range engine.Run(ctx, req) {
		if err != nil {
			return err
		}
	}
	return nil
}

var pairArguments = []string{"one", "two", "three", "four", "five", "six"}

func repeatedIDResponses(call func(id, argument string) *genai.Part) [][]*genai.Part {
	var responses [][]*genai.Part
	for turnIndex := range 2 {
		args := pairArguments[turnIndex*3 : turnIndex*3+3]
		responses = append(responses,
			[]*genai.Part{call("call_0", args[0]), call("call_0", args[1])},
			[]*genai.Part{call("call_0", args[2])},
			textResponse(),
		)
	}
	return responses
}

func runTwoTurnsInMemory(t *testing.T, model *scriptedModel, effectful bool) *effectTools {
	t.Helper()
	rig := newMemoryRig(t, model)
	var effects *effectTools
	if effectful {
		effects = newEffectTools(t, rig.db)
		rig.broker, rig.builtins = effects, effects
	} else {
		rig.builtins = &toolProbe{}
	}
	engine := rig.engine(t)
	for _, turnID := range []string{"turn-1", "turn-2"} {
		if err := runEngineTurn(t.Context(), engine, ownerTurn(turnID)); err != nil {
			t.Fatalf("run %s: %v", turnID, err)
		}
	}
	return effects
}

func runTwoTurnsDurably(t *testing.T, model *scriptedModel, effectful bool) *effectTools {
	t.Helper()
	rig := newDurableTurnRig(t, model)
	var effects *effectTools
	if effectful {
		effects = newEffectTools(t, rig.db)
		rig.tools = effects
	} else {
		gated := newGatedTools(t)
		close(gated.release)
		rig.tools = gated
	}
	rig.startLife(t)
	for _, turnID := range []string{"turn-1", "turn-2"} {
		awaitStream(t, rig.runRequest(context.WithoutCancel(t.Context()), ownerTurn(turnID)))
	}
	return effects
}

func requireDistinctPairs(t *testing.T, request requestParts, field string) []string {
	t.Helper()
	var calls, responses []requestPart
	for _, part := range request {
		if isCall(part) {
			calls = append(calls, part)
		}
		if isResponse(part) {
			responses = append(responses, part)
		}
	}
	if len(calls) != len(pairArguments) || len(responses) != len(pairArguments) {
		t.Fatalf("request holds %d calls and %d responses, want %d of each (%+v)", len(calls), len(responses), len(pairArguments), request)
	}
	var ids []string
	for i, call := range calls {
		requireAuraCallID(t, call.callID)
		ids = append(ids, call.callID)
		var outputs []string
		for _, response := range responses {
			if response.responseID == call.callID {
				outputs = append(outputs, response.output)
			}
		}
		want := fmt.Sprintf("%q:%q", field, pairArguments[i])
		if len(outputs) != 1 || !strings.Contains(outputs[0], want) {
			t.Errorf("call %d (%s) is answered by %q, want exactly one response carrying %s", i, call.callID, outputs, want)
		}
	}
	if distinct := len(slices.Compact(slices.Sorted(slices.Values(ids)))); distinct != len(ids) {
		t.Errorf("call ids = %v, want pairwise distinct", ids)
	}
	return ids
}

func TestRepeatedProviderCallIDsReachTheModelAsDistinctPairs(t *testing.T) {
	runners := []struct {
		name string
		run  func(*testing.T, *scriptedModel, bool) *effectTools
	}{
		{name: "in memory", run: runTwoTurnsInMemory},
		{name: "durable", run: runTwoTurnsDurably},
	}
	kinds := []struct {
		name      string
		call      func(id, argument string) *genai.Part
		field     string
		effectful bool
	}{
		{name: "read-only", call: queryCall, field: "query"},
		{name: "effectful", call: effectCall, field: "note", effectful: true},
	}
	for _, runner := range runners {
		t.Run(runner.name, func(t *testing.T) {
			for _, kind := range kinds {
				t.Run(kind.name, func(t *testing.T) {
					model := &scriptedModel{responses: repeatedIDResponses(kind.call)}

					effects := runner.run(t, model, kind.effectful)

					requests := model.seen()
					if len(requests) != 6 {
						t.Fatalf("model calls = %d, want 6", len(requests))
					}
					ids := requireDistinctPairs(t, requests[5], kind.field)
					if !kind.effectful {
						return
					}
					if got := effects.adapterRuns(); got != 6 {
						t.Errorf("adapter runs = %d, want 6", got)
					}
					if failures := effects.seenFailures(); len(failures) != 0 {
						t.Errorf("broker errors = %v, want none", failures)
					}
					var gotIDs, gotKeys, gotStates []string
					for _, intent := range effects.intents(t) {
						gotIDs = append(gotIDs, intent.toolCallID)
						gotKeys = append(gotKeys, intent.key)
						gotStates = append(gotStates, intent.state)
					}
					wantKeys := []string{
						"adk/session-1/turn-1/call-1-0", "adk/session-1/turn-1/call-1-1", "adk/session-1/turn-1/call-2-0",
						"adk/session-1/turn-2/call-1-0", "adk/session-1/turn-2/call-1-1", "adk/session-1/turn-2/call-2-0",
					}
					if !slices.Equal(gotKeys, wantKeys) {
						t.Errorf("intent keys = %v, want %v", gotKeys, wantKeys)
					}
					if !slices.Equal(gotIDs, ids) {
						t.Errorf("intent tool call ids = %v, want the call ids of the request %v", gotIDs, ids)
					}
					for _, state := range gotStates {
						if state != "succeeded" {
							t.Errorf("intent states = %v, want all succeeded", gotStates)
							break
						}
					}
				})
			}
		})
	}
}

func adkEventParts(t *testing.T, events []store.RuntimeEvent) requestParts {
	t.Helper()
	var stored requestParts
	for i := range events {
		if events[i].Kind != store.EventKindADK {
			continue
		}
		ev, err := store.RuntimeEventToADK(&events[i])
		if err != nil {
			t.Fatalf("decode stored ADK event %s: %v", events[i].ID, err)
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
	return stored
}

func callAndResponseIDs(parts requestParts) (callID, responseID string) {
	for _, part := range parts {
		if isCall(part) {
			callID = part.callID
		}
		if isResponse(part) {
			responseID = part.responseID
		}
	}
	return callID, responseID
}

func TestReplayedModelResponseKeepsItsCallIDs(t *testing.T) {
	model := &scriptedModel{responses: [][]*genai.Part{{effectCall("call_0", "one")}, textResponse()}}
	rig := newDurableTurnRig(t, model)
	effects := newEffectTools(t, rig.db)
	effects.blockFirst = true
	rig.tools = effects
	rig.startLife(t)
	rig.run(context.WithoutCancel(t.Context()))
	waitFor(t, "the first life is inside the effect", func() bool { return effects.adapterRuns() == 1 })

	rig.stopHost()
	rig.startLife(t)
	waitFor(t, "the second life ends the turn", func() bool { return rig.turnEnded(t) })

	requests := model.seen()
	if len(requests) != 2 {
		t.Fatalf("model calls over both lives = %d, want 2", len(requests))
	}
	ids := effects.seenRequestIDs()
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("request ids = %v, want the same id in both lives", ids)
	}
	id := ids[0]
	requireAuraCallID(t, id)
	intents := effects.intents(t)
	if len(intents) != 1 || intents[0].toolCallID != id {
		t.Errorf("effect intents = %+v, want one with the tool call id %s", intents, id)
	}
	if callID, responseID := callAndResponseIDs(adkEventParts(t, rig.turnEvents(t))); callID != id || responseID != id {
		t.Errorf("stored call id = %q, response id = %q, want %s on both", callID, responseID, id)
	}
	if callID, responseID := callAndResponseIDs(requests[1]); callID != id || responseID != id {
		t.Errorf("second request call id = %q, response id = %q, want %s on both", callID, responseID, id)
	}
}
