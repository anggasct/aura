package runtimeadk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/durable"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/toolcall"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const overlapBound = 5 * time.Second

type overlapGate struct {
	count    int32
	started  atomic.Int32
	all      chan struct{}
	finished map[string]chan struct{}
	after    map[string]string
}

func newOverlapGate(queries []string) *overlapGate {
	gate := &overlapGate{count: int32(len(queries)), all: make(chan struct{}), finished: map[string]chan struct{}{}, after: map[string]string{}}
	for i, query := range queries {
		gate.finished[query] = make(chan struct{})
		if i+1 < len(queries) {
			gate.after[query] = queries[i+1]
		}
	}
	return gate
}

func (g *overlapGate) pass(ctx context.Context, query string) error {
	if g.started.Add(1) == g.count {
		close(g.all)
	}
	bound := time.After(overlapBound)
	select {
	case <-g.all:
	case <-ctx.Done():
		return ctx.Err()
	case <-bound:
		return errors.New("the other calls never started")
	}
	if later, ok := g.after[query]; ok {
		select {
		case <-g.finished[later]:
		case <-ctx.Done():
			return ctx.Err()
		case <-bound:
			return errors.New("the later calls never finished")
		}
	}
	close(g.finished[query])
	return nil
}

type toolProbe struct {
	hold func(ctx context.Context, request *BuiltinToolRequest, query string) error

	mu      sync.Mutex
	running int
	peak    int
	started []string
}

func (p *toolProbe) Definitions() []BuiltinToolDefinition {
	return []BuiltinToolDefinition{{Name: "tool-a", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`)}}
}

func (p *toolProbe) Execute(ctx context.Context, request *BuiltinToolRequest) (json.RawMessage, error) {
	var arguments struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.running++
	p.peak = max(p.peak, p.running)
	p.started = append(p.started, arguments.Query)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running--
		p.mu.Unlock()
	}()
	if p.hold != nil {
		if err := p.hold(ctx, request, arguments.Query); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]string{"query": arguments.Query})
}

func (p *toolProbe) snapshot() (running, peak int, started []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running, p.peak, slices.Clone(p.started)
}

type answerAfterToolModel struct{}

func (m *answerAfterToolModel) Name() string { return "answer-after-tool-model" }

func (m *answerAfterToolModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		answered := false
		for _, content := range req.Contents {
			for _, part := range content.Parts {
				answered = answered || part.FunctionResponse != nil
			}
		}
		parts := []*genai.Part{{Text: "done"}}
		if !answered {
			parts = []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "tool-a", Args: map[string]any{"query": "x"}}}}
		}
		yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}, TurnComplete: true}, nil)
	}
}

func queriesOf(count int) []string {
	queries := make([]string, count)
	for i := range queries {
		queries[i] = fmt.Sprintf("q%02d", i)
	}
	return queries
}

func memoryExecutor(t *testing.T, model adkmodel.LLM, builtins BuiltinToolExecutor, sessionIDs ...string) *ADKExecutor {
	t.Helper()
	db, sessions, _ := newSessionTestDB(t)
	for _, id := range sessionIDs {
		mustCreateSession(t, db, id)
	}
	executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtins))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	return executor
}

func sessionTurn(sessionID string) *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID: "turn-" + sessionID, SessionID: sessionID, PrincipalID: "user-1", Origin: turn.OriginTerminal,
		Parts: []runtimeingress.InputPart{{Text: durableTurnText}},
	}
}

func runTurn(ctx context.Context, executor *ADKExecutor, req *turn.TurnRequest) error {
	for _, err := range executor.Execute(ctx, req) {
		if err != nil {
			return err
		}
	}
	return nil
}

func responseOutputs(request requestParts) []string {
	var outputs []string
	for _, part := range request {
		if isResponse(part) {
			outputs = append(outputs, part.output)
		}
	}
	return outputs
}

func requireOutputsInCallOrder(t *testing.T, request requestParts, queries []string) {
	t.Helper()
	outputs := responseOutputs(request)
	if len(outputs) != len(queries) {
		t.Fatalf("function responses in the next request = %d, want %d (%q)", len(outputs), len(queries), outputs)
	}
	for i, query := range queries {
		if want := fmt.Sprintf(`"query":%q`, query); !strings.Contains(outputs[i], want) {
			t.Errorf("function response %d = %s, want the output of %s", i, outputs[i], query)
		}
	}
}

func TestToolCallsOfOneResponseRunSideBySide(t *testing.T) {
	queries := []string{"a", "b", "c"}
	t.Run("a durable turn", func(t *testing.T) {
		model := &toolThenTextModel{queries: queries}
		rig := startDurableTurnRig(t, model)
		rig.tools.overlap = newOverlapGate(queries)
		close(rig.tools.release)

		awaitStream(t, rig.run(context.WithoutCancel(t.Context())))

		requests := model.seen()
		if len(requests) != 2 {
			t.Fatalf("model calls = %d, want 2", len(requests))
		}
		requireOutputsInCallOrder(t, requests[1], queries)
	})
	t.Run("in memory", func(t *testing.T) {
		model := &toolThenTextModel{queries: queries}
		gate := newOverlapGate(queries)
		probe := &toolProbe{hold: func(ctx context.Context, _ *BuiltinToolRequest, query string) error { return gate.pass(ctx, query) }}
		executor := memoryExecutor(t, model, probe, "session-1")

		if err := runTurn(t.Context(), executor, sessionTurn("session-1")); err != nil {
			t.Fatalf("Execute: %v", err)
		}

		requests := model.seen()
		if len(requests) != 2 {
			t.Fatalf("model calls = %d, want 2", len(requests))
		}
		requireOutputsInCallOrder(t, requests[1], queries)
	})
}

func TestToolCallsOfOneBatchAreBounded(t *testing.T) {
	queries := queriesOf(12)
	model := &toolThenTextModel{queries: queries}
	var release chan struct{}
	probe := &toolProbe{hold: func(context.Context, *BuiltinToolRequest, string) error {
		<-release
		return nil
	}}
	executor := memoryExecutor(t, model, probe, "session-1")
	synctest.Test(t, func(t *testing.T) {
		release = make(chan struct{})
		releaseAll := sync.OnceFunc(func() { close(release) })
		defer releaseAll()
		done := make(chan error, 1)
		go func() { done <- runTurn(t.Context(), executor, sessionTurn("session-1")) }()

		synctest.Wait()
		running, _, started := probe.snapshot()
		if running != 8 || len(started) != 8 {
			t.Fatalf("calls running = %d, started = %d, want 8 and 8", running, len(started))
		}
		releaseAll()
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if _, peak, started := probe.snapshot(); peak != 8 || len(started) != 12 {
			t.Errorf("peak = %d, started = %d, want 8 and 12", peak, len(started))
		}
		requests := model.seen()
		if len(requests) != 2 {
			t.Fatalf("model calls = %d, want 2", len(requests))
		}
		requireOutputsInCallOrder(t, requests[1], queries)
	})
}

func TestToolCallsRunInlineWithoutConcurrentActions(t *testing.T) {
	queries := []string{"a", "b", "c"}
	model := &toolThenTextModel{queries: queries}
	probe := &toolProbe{hold: func(context.Context, *BuiltinToolRequest, string) error {
		time.Sleep(time.Second)
		return nil
	}}
	executor := memoryExecutor(t, model, probe, "session-1")
	synctest.Test(t, func(t *testing.T) {
		ctx := durable.WithTurnScope(t.Context(), durable.NewTurnScope(&journalInvocation{}))
		if err := runTurn(ctx, executor, sessionTurn("session-1")); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if _, peak, started := probe.snapshot(); peak != 1 || !slices.Equal(started, queries) {
			t.Errorf("peak = %d, started = %q, want 1 and %q", peak, started, queries)
		}
	})
}

func TestToolCallsOfTwoSessionsDoNotWaitForEachOther(t *testing.T) {
	model := &answerAfterToolModel{}
	release := make(chan struct{})
	releaseA := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseA)
	probe := &toolProbe{hold: func(_ context.Context, request *BuiltinToolRequest, _ string) error {
		if request.SessionID == "session-a" {
			<-release
		}
		return nil
	}}
	executor := memoryExecutor(t, model, probe, "session-a", "session-b")
	held := make(chan error, 1)
	go func() { held <- runTurn(t.Context(), executor, sessionTurn("session-a")) }()
	waitFor(t, "session a's call is waiting", func() bool {
		running, _, _ := probe.snapshot()
		return running == 1
	})

	free := make(chan error, 1)
	go func() { free <- runTurn(t.Context(), executor, sessionTurn("session-b")) }()
	select {
	case err := <-free:
		if err != nil {
			t.Fatalf("session b: %v", err)
		}
	case <-time.After(overlapBound):
		t.Fatal("session b is still waiting for the call of session a")
	}

	releaseA()
	if err := <-held; err != nil {
		t.Fatalf("session a: %v", err)
	}
}

func panicInTask(context.Context) {
	panic(errPanickedTask)
}

var errPanickedTask = errors.New("the task panicked")

func runnerCtx(t *testing.T) context.Context {
	t.Helper()
	return platform.WithTaskRunner(t.Context(), newToolCallRunner().run)
}

func runTasksWithin(ctx context.Context, t *testing.T, tasks []func(context.Context)) (raised any) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		platform.RunTasks(ctx, tasks)
	}()
	select {
	case raised = <-done:
		return raised
	case <-time.After(overlapBound):
		t.Fatal("RunTasks never returned")
		return nil
	}
}

func TestToolCallRunnerCallsEveryTaskAndRaisesItsPanic(t *testing.T) {
	t.Run("an ended context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(runnerCtx(t))
		cancel()
		var called atomic.Int32
		tasks := make([]func(context.Context), 12)
		for i := range tasks {
			tasks[i] = func(context.Context) { called.Add(1) }
		}

		if raised := runTasksWithin(ctx, t, tasks); raised != nil {
			t.Fatalf("RunTasks panicked with %v", raised)
		}

		if got := called.Load(); got != 12 {
			t.Errorf("tasks called = %d, want 12", got)
		}
	})
	t.Run("a panic, concurrent", func(t *testing.T) {
		var waited, cancelled atomic.Bool
		tasks := []func(context.Context){
			panicInTask,
			func(ctx context.Context) {
				slot, _ := toolcall.From(ctx)
				waited.Store(slot.AwaitAuthorization(context.WithoutCancel(ctx)) == nil)
			},
			func(ctx context.Context) {
				<-ctx.Done()
				cancelled.Store(true)
			},
		}

		raised := runTasksWithin(runnerCtx(t), t, tasks)

		wrapped, ok := raised.(*toolCallPanic)
		if !ok {
			t.Fatalf("RunTasks panicked with %T %v, want *toolCallPanic", raised, raised)
		}
		if !errors.Is(asError(wrapped.value), errPanickedTask) {
			t.Errorf("panic value = %v, want %v", wrapped.value, errPanickedTask)
		}
		if !bytes.Contains(wrapped.stack, []byte("panicInTask")) {
			t.Errorf("panic stack = %s, want it to name the panicking task", wrapped.stack)
		}
		if text := wrapped.String(); !strings.Contains(text, errPanickedTask.Error()) || !strings.Contains(text, "panicInTask") {
			t.Errorf("String() = %q, want the value and the stack", text)
		}
		if !waited.Load() {
			t.Error("the call behind the panicking call never saw its slot released")
		}
		if !cancelled.Load() {
			t.Error("the batch context was not cancelled by the panic")
		}
	})
	t.Run("a panic, inline", func(t *testing.T) {
		ctx := durable.WithTurnScope(runnerCtx(t), durable.NewTurnScope(&journalInvocation{}))
		var second atomic.Bool
		tasks := []func(context.Context){panicInTask, func(context.Context) { second.Store(true) }}

		raised := runTasksWithin(ctx, t, tasks)

		if !errors.Is(asError(raised), errPanickedTask) {
			t.Errorf("RunTasks panicked with %T %v, want the original value", raised, raised)
		}
		if second.Load() {
			t.Error("a task ran after the panic")
		}
	})
}

func asError(value any) error {
	err, _ := value.(error)
	return err
}
