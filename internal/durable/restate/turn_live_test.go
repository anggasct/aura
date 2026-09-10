//go:build durable

package restate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	runtimeadk "github.com/anggasct/aura/internal/runtime/adk"
	runtimeengine "github.com/anggasct/aura/internal/runtime/engine"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	runtimesessions "github.com/anggasct/aura/internal/runtime/sessions"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

type liveScriptModel struct {
	mu    sync.Mutex
	calls int
	steps []liveModelStep
}

type liveModelStep struct {
	tool string
	text string
}

func (m *liveScriptModel) Name() string { return "live-script-model" }

func (m *liveScriptModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls++
		call := m.calls
		m.mu.Unlock()
		var parts []*genai.Part
		if call <= len(m.steps) && m.steps[call-1].tool != "" {
			parts = []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   fmt.Sprintf("call-%d", call),
					Name: m.steps[call-1].tool,
					Args: map[string]any{"query": "x"},
				},
			}}
		} else {
			text := "done"
			if call <= len(m.steps) && m.steps[call-1].text != "" {
				text = m.steps[call-1].text
			}
			parts = []*genai.Part{{Text: text}}
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

func (m *liveScriptModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type liveCountingTools struct {
	broker *toolbroker.Broker
	defs   []runtimeadk.BuiltinToolDefinition
	mu     sync.Mutex
	counts map[string]int
	gates  map[string]string
	dir    string
}

func (l *liveCountingTools) Definitions() []runtimeadk.BuiltinToolDefinition {
	return l.defs
}

func (l *liveCountingTools) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return l.broker.Evaluate(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, RequestDigest: request.RequestDigest, Capabilities: request.Capabilities,
		Trust: request.Trust, Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
	})
}

func (l *liveCountingTools) Execute(ctx context.Context, request *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	l.mu.Lock()
	release := l.gates[request.ToolName]
	entered := l.gates[request.ToolName+":entered"]
	l.mu.Unlock()
	if entered != "" {
		_ = os.WriteFile(entered, []byte("entered"), 0600)
	}
	if release != "" {
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	result, err := l.broker.Execute(ctx, &toolbroker.ToolRequest{
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

func (l *liveCountingTools) doneContent(name string) string {
	raw, err := os.ReadFile(filepath.Join(l.dir, name+".done"))
	if err != nil {
		return ""
	}
	return string(raw)
}

func liveExecutions(t *testing.T, tools *liveCountingTools, name string) {
	t.Helper()
	tools.mu.Lock()
	defer tools.mu.Unlock()
	tools.counts[name]++
	_ = os.WriteFile(filepath.Join(tools.dir, name+".done"), []byte(`{"ok":true}`), 0600)
}

func (l *liveCountingTools) count(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[name]
}

type liveTurnRig struct {
	t           *testing.T
	db          *sql.DB
	events      store.EventStore
	adapter     *Adapter
	engine      *runtimeengine.Engine
	model       *liveScriptModel
	tools       *liveCountingTools
	server      *liveServer
	dir         string
	ingressPort int
	adminPort   int
	handlerAddr string
	endpoint    *Endpoint
	stop        context.CancelFunc
	done        chan error
}

func startLiveTurnRig(t *testing.T, binary string, model *liveScriptModel, toolDefs []liveToolDef) *liveTurnRig {
	t.Helper()
	ctx := context.Background()
	db := liveTestDB(t)
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: "session-1", OwnerID: "user-1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	events := store.NewEventStore(db)
	dedupe := store.NewDedupeStore(db)

	counting := &liveCountingTools{
		counts: map[string]int{},
		gates:  map[string]string{},
		dir:    t.TempDir(),
	}
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	for _, def := range toolDefs {
		toolDef := def
		adapter := func(ctx context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			liveExecutions(t, counting, toolDef.name)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		}
		if err := broker.RegisterTool(
			&tools.Definition{Name: toolDef.name, Version: "v1", Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				return raw, nil
			}},
			adapter,
			&approval.Rule{ToolName: toolDef.name, ToolVersion: "v1", RequiresApproval: toolDef.approval},
		); err != nil {
			t.Fatalf("register tool %s: %v", toolDef.name, err)
		}
		counting.defs = append(counting.defs, runtimeadk.BuiltinToolDefinition{
			Name:    toolDef.name,
			Version: "v1",
			Schema:  json.RawMessage(`{"type":"object"}`),
		})
	}
	counting.broker = broker

	modelName := "live-model-" + fmt.Sprint(time.Now().UnixNano())
	adkmodel.Register("^"+modelName+"$", func(context.Context, string) (adkmodel.LLM, error) {
		return model, nil
	})
	executor, err := runtimeadk.NewADKExecutor("aura", modelName, store.NewSessionService(db), events, counting, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(counting),
	)
	if err != nil {
		t.Fatalf("new ADK executor: %v", err)
	}

	dir := t.TempDir()
	ingressPort, adminPort := freePort(t), freePort(t)
	handlerPort := freePort(t)
	handlerAddr := fmt.Sprintf("127.0.0.1:%d", handlerPort)
	ingressURL := fmt.Sprintf("http://127.0.0.1:%d", ingressPort)

	server := startLiveServer(t, binary, dir, ingressPort, adminPort)
	t.Cleanup(func() { server.stop() })

	adapter, err := NewAdapter(Config{IngressURL: ingressURL}, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	sessionCalls := &liveSessionCalls{runtime: adapter}
	engine, err := runtimeengine.NewEngine(runtimeengine.Config{
		MaxActiveTurns:  4,
		MaxPendingTurns: 16,
		TurnTimeout:     5 * time.Minute,
		DefaultAgentID:  "main",
		Durable: &runtimeengine.DurableConfig{
			Sessions: sessionCalls,
			Runtime:  adapter,
		},
	}, events, dedupe, executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	executor.SetEventPublisher(engine)
	engine.MarkRecovered()

	rig := &liveTurnRig{
		t: t, db: db, events: events, adapter: adapter,
		engine: engine, model: model, tools: counting,
		server: server, dir: dir,
		ingressPort: ingressPort, adminPort: adminPort,
		handlerAddr: handlerAddr,
	}
	t.Cleanup(func() { rig.server.stop() })
	rig.startEndpoint(handlerAddr)
	return rig
}

type liveToolDef struct {
	name     string
	approval bool
}

type liveSessionCalls struct {
	runtime durable.Runtime
}

func (l *liveSessionCalls) Admit(ctx context.Context, req *runtimesessions.AdmitRequest) (runtimesessions.AdmitResult, error) {
	return liveSessionCall[runtimesessions.AdmitResult](ctx, l.runtime, req.Turn.SessionID, SessionAdmitHandler, req)
}

func (l *liveSessionCalls) Release(ctx context.Context, sessionID, turnID string) (runtimesessions.ReleaseResult, error) {
	return liveSessionCall[runtimesessions.ReleaseResult](ctx, l.runtime, sessionID, SessionReleaseHandler, runtimesessions.ReleaseRequest{TurnID: turnID})
}

func (l *liveSessionCalls) Recover(ctx context.Context, sessionID string, open, terminal []string) (runtimesessions.RecoverResult, error) {
	return liveSessionCall[runtimesessions.RecoverResult](ctx, l.runtime, sessionID, SessionRecoverHandler, runtimesessions.RecoverRequest{Open: open, Terminal: terminal})
}

func (l *liveSessionCalls) Abort(ctx context.Context, sessionID string) (runtimesessions.AbortResult, error) {
	return liveSessionCall[runtimesessions.AbortResult](ctx, l.runtime, sessionID, SessionAbortHandler, json.RawMessage(`{}`))
}

func liveSessionCall[Result any](ctx context.Context, rt durable.Runtime, sessionID, handler string, payload any) (Result, error) {
	var zero Result
	raw, err := json.Marshal(payload)
	if err != nil {
		return zero, err
	}
	out, err := rt.Call(ctx, durable.CallRequest{Service: SessionServiceName, Key: sessionID, Handler: handler, Payload: raw})
	if err != nil {
		return zero, err
	}
	var result Result
	if err := json.Unmarshal(out, &result); err != nil {
		return zero, err
	}
	return result, nil
}

func (r *liveTurnRig) startEndpoint(handlerAddr string) {
	r.t.Helper()
	endpoint, err := NewEndpoint(EndpointConfig{HandlerAddr: handlerAddr}, nil)
	if err != nil {
		r.t.Fatalf("NewEndpoint: %v", err)
	}
	endpoint.RegisterSessionTurns(SessionStores{Events: r.events, Dedupe: store.NewDedupeStore(r.db)})
	endpoint.RegisterHandler("turn", func(ctx context.Context, inv durable.Invocation) error {
		var desc runtimesessions.Descriptor
		if err := json.Unmarshal(inv.Payload(), &desc); err != nil {
			return fmt.Errorf("decode turn payload: %w", err)
		}
		return r.engine.DriveTurn(ctx, inv, &desc)
	})
	endpoint.RegisterHandler("workflow", func(context.Context, durable.Invocation) error { return nil })
	endpointCtx, stop := context.WithCancel(context.Background())
	r.stop = stop
	done := make(chan error, 1)
	go func() { done <- endpoint.Start(endpointCtx); close(done) }()
	r.done = done
	r.endpoint = endpoint
	waitEndpointReady(r.t, endpoint)
	r.server.registerDeployment(r.t, handlerAddr)
}

func (r *liveTurnRig) crash() {
	r.t.Helper()
	r.stop()
	for range r.done {
	}
	r.server.stop()
	r.t.Logf("phase=crashed")
}

func (r *liveTurnRig) restart() {
	r.t.Helper()
	r.server = startLiveServer(r.t, r.server.binary, r.dir, r.ingressPort, r.adminPort)
	r.startEndpoint(r.handlerAddr)
	r.t.Logf("phase=restarted")
}

func (r *liveTurnRig) waitFile(path string, within time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("flag file %s never appeared", path)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (r *liveTurnRig) waitTerminal(turnID string, within time.Duration) []store.RuntimeEvent {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for {
		events, err := store.NewDedupeStore(r.db).ListTurnEvents(context.Background(), turnID)
		if err != nil {
			r.t.Fatalf("list turn events: %v", err)
		}
		for i := range events {
			switch events[i].Kind {
			case runtime.EventKindTurnCompleted, runtime.EventKindTurnFailed, runtime.EventKindTurnCancelled:
				return events
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("turn %s never reached a terminal event", turnID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (r *liveTurnRig) countKind(turnID, kind string) int {
	r.t.Helper()
	events, err := store.NewDedupeStore(r.db).ListTurnEvents(context.Background(), turnID)
	if err != nil {
		r.t.Fatalf("list turn events: %v", err)
	}
	count := 0
	for i := range events {
		if events[i].Kind == kind {
			count++
		}
	}
	return count
}

func liveTurnRequest(turnID string) *runtime.TurnRequest {
	return &runtime.TurnRequest{
		TurnID:      turnID,
		SessionID:   "session-1",
		PrincipalID: "user-1",
		Origin:      runtime.OriginTerminal,
		Parts:       []runtimeingress.InputPart{{Text: "go"}},
	}
}

func TestLiveTurnToolsSurviveCrash(t *testing.T) {
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("restate-server binary is not available: %v", err)
	}
	model := &liveScriptModel{steps: []liveModelStep{{tool: "tool-a"}, {tool: "tool-b"}, {text: "done"}}}
	rig := startLiveTurnRig(t, binary, model, []liveToolDef{{name: "tool-a"}, {name: "tool-b"}})
	entered := filepath.Join(t.TempDir(), "tool-b-entered")
	release := filepath.Join(t.TempDir(), "tool-b-release")
	rig.tools.gates["tool-b:entered"] = entered
	rig.tools.gates["tool-b"] = release

	streamDone := make(chan error, 1)
	go func() {
		for ev, err := range rig.engine.Run(context.Background(), liveTurnRequest("turn-1")) {
			_ = ev
			if err != nil {
				streamDone <- err
				return
			}
		}
		streamDone <- nil
	}()

	rig.waitFile(entered, 90*time.Second)
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls at kill point = %d, want 2", got)
	}
	rig.crash()
	rig.restart()
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatalf("write release flag: %v", err)
	}

	events := rig.waitTerminal("turn-1", 120*time.Second)
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
	if got := model.callCount(); got != 3 {
		t.Fatalf("model calls = %d, want 3 (no model re-invocation across resume)", got)
	}
	if got := rig.tools.count("tool-a"); got != 1 {
		t.Fatalf("tool-a executions = %d, want 1 (completed intent not re-executed)", got)
	}
	if got := rig.tools.count("tool-b"); got != 1 {
		t.Fatalf("tool-b executions = %d, want 1 (interrupted attempt never began journaled work)", got)
	}
	if got := rig.tools.doneContent("tool-a"); got != `{"ok":true}` {
		t.Fatalf("tool-a output = %q, want recorded success (proof the call completed, not just started)", got)
	}
	if got := rig.tools.doneContent("tool-b"); got != `{"ok":true}` {
		t.Fatalf("tool-b output = %q, want recorded success", got)
	}
}

func TestLiveTurnApprovalSurvivesCrash(t *testing.T) {
	binary := os.Getenv("AURA_RESTATE_BINARY")
	if binary == "" {
		t.Skip("AURA_RESTATE_BINARY is not set")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("restate-server binary is not available: %v", err)
	}
	model := &liveScriptModel{steps: []liveModelStep{{tool: "tool-c"}, {text: "done"}}}
	rig := startLiveTurnRig(t, binary, model, []liveToolDef{{name: "tool-c", approval: true}})

	streamDone := make(chan error, 1)
	go func() {
		for ev, err := range rig.engine.Run(context.Background(), liveTurnRequest("turn-2")) {
			_ = ev
			if err != nil {
				streamDone <- err
				return
			}
		}
		streamDone <- nil
	}()

	addr := rig.waitApprovalEvent("turn-2", 90*time.Second)
	if got := rig.countKind("turn-2", runtime.EventKindApprovalRequired); got != 1 {
		t.Fatalf("approval.required rows = %d, want 1", got)
	}
	rig.crash()
	rig.restart()
	if got := rig.countKind("turn-2", runtime.EventKindApprovalRequired); got != 1 {
		t.Fatalf("approval.required rows after resume = %d, want 1 (no duplicate emit)", got)
	}

	decision, _ := json.Marshal(map[string]any{"approved": true})
	if err := rig.adapter.ResolveApproval(context.Background(), durable.ResolveApprovalRequest{
		ApprovalID: addr,
		Payload:    decision,
	}); err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	events := rig.waitTerminal("turn-2", 120*time.Second)
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
	if got := rig.tools.count("tool-c"); got != 1 {
		t.Fatalf("tool-c calls = %d, want 1", got)
	}
	if got := rig.tools.doneContent("tool-c"); got != `{"ok":true}` {
		t.Fatalf("tool-c output = %q, want recorded success", got)
	}
	if err := rig.adapter.ResolveApproval(context.Background(), durable.ResolveApprovalRequest{
		ApprovalID: addr,
		Payload:    decision,
	}); err == nil {
		t.Fatal("expected a double-consume error")
	}
}

func (r *liveTurnRig) waitApprovalEvent(turnID string, within time.Duration) string {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for {
		events, err := store.NewDedupeStore(r.db).ListTurnEvents(context.Background(), turnID)
		if err != nil {
			r.t.Fatalf("list turn events: %v", err)
		}
		for i := range events {
			if events[i].Kind == runtime.EventKindApprovalRequired {
				var payload struct {
					ApprovalID string `json:"approval_id"`
				}
				if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
					r.t.Fatalf("decode approval event: %v", err)
				}
				if payload.ApprovalID == "" {
					r.t.Fatal("approval event carries no approval id")
				}
				return payload.ApprovalID
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("turn %s never parked for approval", turnID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
