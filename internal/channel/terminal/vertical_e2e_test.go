//go:build e2e

package terminal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// e2eApprovalModel requests the exec tool on the first call and answers with
// fixed text afterwards, mirroring a provider that runs one tool then
// finishes the turn.
type e2eApprovalModel struct {
	mu       sync.Mutex
	calls    int
	toolName string
	answer   string
}

func (m *e2eApprovalModel) Name() string { return "e2e-approval-model" }

func (m *e2eApprovalModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls++
		first := m.calls == 1
		toolName := m.toolName
		answer := m.answer
		m.mu.Unlock()
		parts := []*genai.Part{{Text: answer}}
		if first {
			parts = []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call-e2e-1",
					Name: toolName,
					Args: map[string]any{"command": []any{"echo", "e2e"}},
				},
			}}
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
		}, nil)
	}
}

func registerE2EModel(t *testing.T, model adkmodel.LLM) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("model name entropy: %v", err)
	}
	name := "e2e-model-" + hex.EncodeToString(b[:])
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) {
		return model, nil
	})
	return name
}

// e2eEffectPublisher adapts a publish function onto the journal publisher.
type e2eEffectPublisher func(*store.RuntimeEvent)

func (f e2eEffectPublisher) Publish(ev *store.RuntimeEvent) { f(ev) }

// e2eBuiltinTools exposes a real tool broker as the runtime builtin executor.
type e2eBuiltinTools struct {
	broker  *toolbroker.Broker
	journal *effect.Journal
}

func (b e2eBuiltinTools) SetEventPublisher(publish func(*store.RuntimeEvent)) {
	if publish == nil {
		return
	}
	b.journal.SetEventPublisher(e2eEffectPublisher(publish))
}

func (b e2eBuiltinTools) Definitions() []runtime.BuiltinToolDefinition {
	definitions := b.broker.Definitions()
	out := make([]runtime.BuiltinToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, runtime.BuiltinToolDefinition{
			Name:                 definition.Name,
			Version:              definition.Version,
			Description:          "Aura built-in " + definition.Name + " tool",
			Schema:               definition.Schema,
			RequiredCapabilities: definition.RequiredCapabilities,
		})
	}
	return out
}

func (b e2eBuiltinTools) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return b.broker.Evaluate(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, RequestDigest: request.RequestDigest, Capabilities: request.Capabilities,
		Trust: request.Trust, Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
	})
}

func (b e2eBuiltinTools) Execute(ctx context.Context, request *runtime.BuiltinToolRequest) (json.RawMessage, error) {
	result, err := b.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, Capabilities: request.Capabilities,
		Trust: approval.TrustLabel(request.Trust), Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
		EventSequence: request.EventSequence, EventInvocation: request.EventInvocation,
		EventBranch: request.EventBranch, EventAuthor: request.EventAuthor,
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

// approvalE2EStack is a full vertical slice over the real engine, ADK
// executor, effect journal, and tool broker, with only the model provider
// and the exec adapter replaced by fakes.
type approvalE2EStack struct {
	engine   runtime.AgentRuntime
	sessions store.SessionService
	calls    *atomic.Int32
}

func newApprovalE2EStack(t *testing.T, decider toolbroker.ApprovalDecider, answer string) *approvalE2EStack {
	t.Helper()
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("effect executor: %v", err)
	}
	calls := &atomic.Int32{}
	broker, err := toolbroker.New(&toolbroker.Options{
		Effects: effects,
		Adapters: map[string]toolbroker.Adapter{
			"exec@v1": func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				calls.Add(1)
				return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			},
		},
		ApprovalDecider: decider,
	})
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	builtin := e2eBuiltinTools{broker: broker, journal: journal}
	model := &e2eApprovalModel{toolName: "exec", answer: answer}
	modelName := registerE2EModel(t, model)
	executor, err := runtime.NewADKExecutor("aura", modelName, sessions, events, builtin, nil, nil, runtime.WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	engine, err := runtime.NewEngine(runtime.Config{}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	executor.SetEventPublisher(engine)
	return &approvalE2EStack{engine: engine, sessions: sessions, calls: calls}
}

func newApprovalE2EConsole(t *testing.T, stack *approvalE2EStack, bridge *terminal.ApprovalBridge, input string) (*bytes.Buffer, *bytes.Buffer, *terminal.Console) {
	t.Helper()
	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := terminal.NewConsole(
		e2eRunner{engine: stack.engine},
		e2eSessions{sessions: stack.sessions},
		terminal.PlainRenderer{},
		bytes.NewBufferString(input),
		out, diag,
		terminal.Config{MaxInputBytes: 4096, InMemoryHistory: 100, SecondInterruptTime: time.Second},
		"e2e-owner",
	)
	console.SetTTY(terminal.NewTTYRenderer(terminal.TTYOptions{
		Out:     e2eWriteCloser{Writer: out},
		Width:   func() int { return 80 },
		Hz:      500,
		Styling: false,
	}))
	if bridge != nil {
		console.SetApprovalBridge(bridge)
	}
	return out, diag, console
}

func e2eDecider(bridge *terminal.ApprovalBridge) toolbroker.ApprovalDecider {
	return func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if prompt == nil {
			return false, nil
		}
		return bridge.Decide(ctx, &terminal.ApprovalCard{
			ToolName:       prompt.ToolName,
			ToolVersion:    prompt.ToolVersion,
			SessionID:      prompt.SessionID,
			TurnID:         prompt.TurnID,
			PrincipalID:    prompt.PrincipalID,
			Arguments:      prompt.Arguments,
			Network:        prompt.Network,
			Timeout:        prompt.Timeout,
			MaxOutputBytes: prompt.MaxOutputBytes,
			PolicyVersion:  prompt.PolicyVersion,
			ReasonCode:     prompt.ReasonCode,
			ExpiresAt:      prompt.ExpiresAt,
		})
	}
}

func TestTerminalApprovalAcceptVerticalSlice(t *testing.T) {
	bridge := terminal.NewApprovalBridge()
	stack := newApprovalE2EStack(t, e2eDecider(bridge), "tool phase done")
	out, _, console := newApprovalE2EConsole(t, stack, bridge, "run it\ny\n")
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("console run: %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "approval required: exec@v1") {
		t.Errorf("output = %q, want the approval card", rendered)
	}
	if !strings.Contains(rendered, `arguments: {"command":["echo","e2e"]}`) {
		t.Errorf("output = %q, want canonical arguments on the card", rendered)
	}
	if !strings.Contains(rendered, "approval accepted: exec@v1") {
		t.Errorf("output = %q, want accepted outcome", rendered)
	}
	if !strings.Contains(rendered, "tool phase done") {
		t.Errorf("output = %q, want the final assistant answer", rendered)
	}
	if stack.calls.Load() != 1 {
		t.Fatalf("adapter calls = %d, want 1", stack.calls.Load())
	}
	stored, err := stack.sessions.ListEvents(context.Background(), "e2e-session", 0, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var toolRequested bool
	for _, ev := range stored {
		if ev.Kind == "tool.requested" {
			toolRequested = true
		}
	}
	if !toolRequested {
		t.Errorf("durable events lack tool.requested: %+v", stored)
	}
}

func TestTerminalApprovalRejectVerticalSlice(t *testing.T) {
	bridge := terminal.NewApprovalBridge()
	stack := newApprovalE2EStack(t, e2eDecider(bridge), "rejected then done")
	out, _, console := newApprovalE2EConsole(t, stack, bridge, "run it\nn\n")
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("console run: %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "approval required: exec@v1") {
		t.Errorf("output = %q, want the approval card", rendered)
	}
	if !strings.Contains(rendered, "approval rejected: exec@v1") {
		t.Errorf("output = %q, want rejected outcome", rendered)
	}
	if stack.calls.Load() != 0 {
		t.Fatalf("adapter calls = %d, want 0 after rejection", stack.calls.Load())
	}
}

func TestTerminalApprovalDefaultRejectVerticalSlice(t *testing.T) {
	bridge := terminal.NewApprovalBridge()
	stack := newApprovalE2EStack(t, e2eDecider(bridge), "default denied")
	out, _, console := newApprovalE2EConsole(t, stack, bridge, "run it\n\n")
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("console run: %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "approval rejected: exec@v1") {
		t.Errorf("output = %q, want default rejection", rendered)
	}
	if strings.Contains(rendered, "approval accepted") {
		t.Errorf("output = %q, empty answer must never accept", rendered)
	}
	if stack.calls.Load() != 0 {
		t.Fatalf("adapter calls = %d, want 0 after default rejection", stack.calls.Load())
	}
}

// blockingExecutor holds turns open until released, seeding engine load.
type blockingExecutor struct {
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	failText string
}

func (b *blockingExecutor) Execute(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		b.once.Do(func() { close(b.started) })
		select {
		case <-b.release:
		case <-ctx.Done():
			yield(store.RuntimeEvent{}, ctx.Err())
			return
		}
		payload, err := json.Marshal(map[string]any{"text": b.failText})
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		if !yield(store.RuntimeEvent{Kind: store.EventKindADK, Payload: payload}, nil) {
			return
		}
		yield(store.RuntimeEvent{Kind: runtime.EventKindTurnCompleted}, nil)
	}
}

func TestTerminalOverloadSurfacesError(t *testing.T) {
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	now := time.Now().UTC()
	for _, id := range []string{"seed-a", "seed-b"} {
		if err := sessions.Create(context.Background(), &store.Session{ID: id, OwnerID: "e2e-owner", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}
	blocker := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	engine, err := runtime.NewEngine(runtime.Config{MaxActiveTurns: 1, MaxPendingTurns: 1}, events, store.NewDedupeStore(db), blocker, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	seedDone := make(chan struct{})
	go func() {
		defer close(seedDone)
		for _, err := range engine.Run(context.Background(), &runtime.TurnRequest{SessionID: "seed-a", PrincipalID: "e2e-owner", Origin: runtime.OriginTerminal, Parts: []runtime.InputPart{{Text: "one"}}}) {
			if err != nil {
				return
			}
		}
	}()
	<-blocker.started
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		for _, err := range engine.Run(context.Background(), &runtime.TurnRequest{SessionID: "seed-b", PrincipalID: "e2e-owner", Origin: runtime.OriginTerminal, Parts: []runtime.InputPart{{Text: "two"}}}) {
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)

	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := terminal.NewConsole(
		e2eRunner{engine: engine},
		e2eSessions{sessions: sessions},
		terminal.PlainRenderer{},
		bytes.NewBufferString("hi\n"),
		out, diag,
		terminal.Config{MaxInputBytes: 1024, InMemoryHistory: 100, SecondInterruptTime: time.Second},
		"e2e-owner",
	)
	err = console.Run(context.Background())
	if err == nil {
		t.Fatal("overloaded submission must surface an error")
	}
	if code, ok := runtime.CodeOf(errorsUnwrapAll(err)); !ok || code != runtime.ErrorCodeRuntimeOverloaded {
		t.Fatalf("error = %v, want runtime_overloaded", err)
	}
	if !strings.Contains(diag.String(), "aura:") {
		t.Errorf("diag = %q, want the runtime diagnostic", diag.String())
	}
	close(blocker.release)
	<-seedDone
}

func errorsUnwrapAll(err error) error { return err }

func TestTerminalRestartResume(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aura.db")

	db1, err := store.OpenDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open first process store: %v", err)
	}
	if err := store.Migrate(context.Background(), db1); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions1 := store.NewSessionService(db1)
	events1 := store.NewEventStore(db1)
	executor1 := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: store.EventKindADK, Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"first durable answer"}]},"partial":false}`)},
	})
	engine1, err := runtime.NewEngine(runtime.Config{}, events1, store.NewDedupeStore(db1), executor1, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	out1 := &bytes.Buffer{}
	console1 := terminal.NewConsole(
		e2eRunner{engine: engine1},
		e2eSessions{sessions: sessions1},
		terminal.PlainRenderer{},
		bytes.NewBufferString("hello\n"),
		out1, &bytes.Buffer{},
		terminal.Config{MaxInputBytes: 1024, InMemoryHistory: 100, SecondInterruptTime: time.Second},
		"e2e-owner",
	)
	if err := console1.Run(context.Background()); err != nil {
		t.Fatalf("first process run: %v", err)
	}
	if !strings.Contains(out1.String(), "first durable answer") {
		t.Fatalf("first process output = %q", out1.String())
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close first process: %v", err)
	}

	db2, err := store.OpenDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open second process store: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	sessions2 := store.NewSessionService(db2)
	events2 := store.NewEventStore(db2)
	executor2 := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: store.EventKindADK, Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"second durable answer"}]},"partial":false}`)},
	})
	engine2, err := runtime.NewEngine(runtime.Config{}, events2, store.NewDedupeStore(db2), executor2, nil)
	if err != nil {
		t.Fatalf("second engine: %v", err)
	}
	out2 := &bytes.Buffer{}
	console2 := terminal.NewConsole(
		e2eRunner{engine: engine2},
		e2eSessions{sessions: sessions2},
		terminal.PlainRenderer{},
		bytes.NewBufferString("again\n"),
		out2, &bytes.Buffer{},
		terminal.Config{MaxInputBytes: 1024, InMemoryHistory: 100, SecondInterruptTime: time.Second},
		"e2e-owner",
	)
	console2.SetSessionID("e2e-session")
	if err := console2.Run(context.Background()); err != nil {
		t.Fatalf("resumed process run: %v", err)
	}
	if !strings.Contains(out2.String(), "second durable answer") {
		t.Fatalf("resumed output = %q, want the second durable answer", out2.String())
	}
	stored, err := sessions2.ListEvents(context.Background(), "e2e-session", 0, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var texts int
	last := uint64(0)
	for _, ev := range stored {
		if ev.Sequence <= last {
			t.Fatalf("sequence %d does not increase after %d", ev.Sequence, last)
		}
		last = ev.Sequence
		if strings.Contains(string(ev.Payload), "durable answer") {
			texts++
		}
	}
	if texts != 2 {
		t.Fatalf("durable answers across restart = %d, want 2", texts)
	}
}

func TestTerminalPrivacyNoPlaintextHistory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "aura.db")
	db, err := store.OpenDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	executor := runtime.NewFakeExecutor([]runtime.FakeStep{
		{Kind: store.EventKindADK, Payload: json.RawMessage(`{"content":{"role":"model","parts":[{"text":"canary-output-7b1c"}]},"partial":false}`)},
	})
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	engine, err := runtime.NewEngine(runtime.Config{}, events, store.NewDedupeStore(db), executor, logger)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	out := &bytes.Buffer{}
	diag := &bytes.Buffer{}
	console := terminal.NewConsole(
		e2eRunner{engine: engine},
		e2eSessions{sessions: sessions},
		terminal.PlainRenderer{},
		bytes.NewBufferString("canary-prompt-9f2a\n"),
		out, diag,
		terminal.Config{MaxInputBytes: 1024, InMemoryHistory: 100, SecondInterruptTime: time.Second},
		"e2e-owner",
	)
	if err := console.Run(context.Background()); err != nil {
		t.Fatalf("console run: %v", err)
	}
	if strings.Contains(logBuf.String(), "canary-prompt-9f2a") || strings.Contains(logBuf.String(), "canary-output-7b1c") {
		t.Errorf("logs leak content: %q", logBuf.String())
	}
	if strings.Contains(diag.String(), "canary-prompt-9f2a") {
		t.Errorf("diagnostics leak the prompt: %q", diag.String())
	}
	allowed := map[string]bool{dbPath: true, dbPath + "-wal": true, dbPath + "-shm": true}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !allowed[path] {
			t.Errorf("unexpected file created beside the store: %s", path)
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file %s has mode %v, want 0600", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk storage dir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat storage dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("storage dir mode = %v, want 0700", info.Mode().Perm())
	}
}
