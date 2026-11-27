package runtimeadk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type budgetScriptStep struct {
	calls []string
	text  string
}

type budgetScriptModel struct {
	steps   []budgetScriptStep
	onStep  func(step int, seen requestParts)
	onCall  func(ctx context.Context, step int, seen requestParts) error
	mu      sync.Mutex
	seen    []requestParts
	callIdx int
}

func (m *budgetScriptModel) Name() string { return "budget-script-model" }

func (m *budgetScriptModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		parts := capturedParts(req)
		m.mu.Lock()
		idx := m.callIdx
		m.callIdx++
		m.seen = append(m.seen, parts)
		m.mu.Unlock()
		if m.onStep != nil {
			m.onStep(idx, parts)
		}
		if m.onCall != nil {
			if err := m.onCall(ctx, idx, parts); err != nil {
				yield(nil, err)
				return
			}
		}
		var reply []*genai.Part
		if idx < len(m.steps) {
			step := m.steps[idx]
			for _, name := range step.calls {
				reply = append(reply, &genai.Part{FunctionCall: &genai.FunctionCall{Name: name, Args: map[string]any{}}})
			}
			if step.text != "" {
				reply = append(reply, &genai.Part{Text: step.text})
			}
		}
		if len(reply) == 0 {
			reply = []*genai.Part{{Text: "done"}}
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: reply},
			TurnComplete: true,
		}, nil)
	}
}

func (m *budgetScriptModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callIdx
}

func (m *budgetScriptModel) requests() []requestParts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]requestParts(nil), m.seen...)
}

type logEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type logCaptureHandler struct {
	mu      sync.Mutex
	entries []logEntry
}

func (h *logCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCaptureHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, logEntry{level: record.Level, msg: record.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *logCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *logCaptureHandler) matching(msg string) []logEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var matched []logEntry
	for _, entry := range h.entries {
		if entry.msg == msg {
			matched = append(matched, entry)
		}
	}
	return matched
}

type captureChannel struct {
	name       string
	mu         sync.Mutex
	deliveries []*runtimechannelhost.DeliveryRequest
}

func (c *captureChannel) ChannelName() string { return c.name }

func (c *captureChannel) Start(context.Context, runtimeingress.IngressSink) error { return nil }

func (c *captureChannel) Deliver(_ context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	c.mu.Lock()
	copyReq := *req
	copyReq.Parts = slices.Clone(req.Parts)
	c.deliveries = append(c.deliveries, &copyReq)
	c.mu.Unlock()
	return runtimechannelhost.ProviderReceipt{ProviderID: c.name, ExternalID: "msg-1", At: time.Now().UTC()}, nil
}

func (c *captureChannel) Health(context.Context) runtimechannelhost.ChannelHealth {
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy}
}

func (c *captureChannel) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, delivery := range c.deliveries {
		for _, part := range delivery.Parts {
			out = append(out, part.Text)
		}
	}
	return out
}

func registerBudgetTestTool(t *testing.T, broker *toolbroker.Broker, name string, rule *approval.Rule, adapter toolbroker.Adapter) {
	t.Helper()
	if rule == nil {
		rule = &approval.Rule{
			ToolName:    name,
			ToolVersion: tools.Version,
			AllowedTrust: []approval.TrustLabel{
				approval.TrustOwnerInput,
				approval.TrustTrustedConfiguration,
				approval.TrustDerivedUntrusted,
			},
		}
	}
	if err := broker.RegisterTool(
		&tools.Definition{
			Name:             name,
			Version:          tools.Version,
			RequiresApproval: rule.RequiresApproval,
			Schema:           json.RawMessage(`{"type":"object"}`),
			Validator:        func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
			Display:          []string{},
		},
		adapter,
		rule,
	); err != nil {
		t.Fatalf("register tool %s: %v", name, err)
	}
}

func lastFunctionResponseOutput(parts requestParts, name string) (string, bool) {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].responseName == name {
			return parts[i].output, true
		}
	}
	return "", false
}

func allFunctionResponses(parts requestParts) []requestPart {
	var out []requestPart
	for _, part := range parts {
		if isResponse(part) {
			out = append(out, part)
		}
	}
	return out
}

func dbActionKeys(t *testing.T, db *sql.DB, turnID string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT action_key FROM durable_action WHERE run_key = ? ORDER BY id`, turnID)
	if err != nil {
		t.Fatalf("query action keys for %s: %v", turnID, err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan action key: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return keys
}

func dbActionResult(t *testing.T, db *sql.DB, turnID, actionKey string) string {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(t.Context(), `SELECT CAST(result AS TEXT) FROM durable_action WHERE run_key = ? AND action_key = ?`, turnID, actionKey).Scan(&raw); err != nil {
		t.Fatalf("read action %s/%s: %v", turnID, actionKey, err)
	}
	return raw
}

func runDurableBudgetTurn(t *testing.T, model adkmodel.LLM, broker *toolbroker.Broker, cfg runtimeengine.Config, logger *slog.Logger, execOpts []ExecutorOption, channels []runtimechannelhost.ChannelPort, req *turn.TurnRequest) ([]turn.Event, *sql.DB) {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	mustCreateSession(t, db, req.SessionID)
	builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	opts := append([]ExecutorOption{
		WithBuiltinToolExecutor(builtin),
		WithToolBudget(ToolBudget{
			AnswerReserve:            time.Duration(cfg.AnswerReserve),
			MaxConsecutiveToolErrors: cfg.MaxConsecutiveToolErrors,
			MaxToolErrorsPerTurn:     cfg.MaxToolErrorsPerTurn,
		}),
	}, execOpts...)
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, logger, opts...)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler), PollInterval: durableTurnPoll, HostTTL: durableTurnHostTTL})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	sessionStore, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db, Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		t.Fatalf("sqlite.NewSessionStore: %v", err)
	}
	eng, err := runtimeengine.New(&runtimeengine.Deps{
		Events:     events,
		Dedupe:     turnstore.NewDedupeStore(db),
		Executor:   executor,
		Logger:     slog.New(slog.DiscardHandler),
		Config:     cfg,
		Durable:    &runtimeengine.DurableConfig{Sessions: sessionStore, Runtime: rt},
		LiveEvents: turn.NewEventBus(nil),
		Channels:   channels,
	})
	if err != nil {
		t.Fatalf("runtimeengine.New: %v", err)
	}
	hostCtx, cancelHost := context.WithCancel(context.WithoutCancel(t.Context()))
	hostDone := make(chan error, 1)
	go func() { hostDone <- rt.Run(hostCtx) }()
	stopHost := sync.OnceFunc(func() {
		cancelHost()
		<-hostDone
	})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = eng.Shutdown(shutdownCtx)
		stopHost()
	})
	var collected []turn.Event
	for ev, err := range eng.Run(t.Context(), req) {
		if err != nil {
			t.Fatalf("engine.Run: %v", err)
		}
		collected = append(collected, ev)
	}
	waitFor(t, "turn released", func() bool {
		return slices.Contains(dbActionKeys(t, db, req.TurnID), "release")
	})
	return collected, db
}

func channelTurnRequest(turnID string) *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID:         turnID,
		SessionID:      "session-1",
		PrincipalID:    principal.OwnerID,
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.Origin("discord:home"),
		ReplyContext:   json.RawMessage(`{"channel":"discord","conversation_id":"chan-1"}`),
		Parts:          []runtimeingress.InputPart{{Text: "do the work"}},
	}
}

func decodeTerminalFailure(t *testing.T, events []turn.Event) (code turn.ErrorCode, detail string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events collected")
	}
	last := events[len(events)-1]
	if last.Kind != turn.EventKindTurnFailed {
		t.Fatalf("terminal event kind = %q, want %q", last.Kind, turn.EventKindTurnFailed)
	}
	var payload struct {
		Code   turn.ErrorCode `json:"code"`
		Detail string         `json:"detail"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	return payload.Code, payload.Detail
}

func TestBudgetRefusesAfterConsecutiveErrorsOfOneTool(t *testing.T) {
	wantRefusal := "tool_budget_exhausted: the tool tool_a did not succeed 3 times in a row. Do not call any more tools in this turn. Reply to the user now, in their language: what you tried, why it did not succeed, and what they can do next."

	for _, fourthTool := range []string{"tool_a", "tool_b"} {
		t.Run("fourth call "+fourthTool+" is refused without policy or approval", func(t *testing.T) {
			var approvalPrompts atomic.Int64
			var aRuns atomic.Int64
			var bRuns atomic.Int64
			broker, err := toolbroker.New(&toolbroker.Options{
				HostCapabilities: capabilitytest.Host(t),
				ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
					approvalPrompts.Add(1)
					return true, nil
				},
			})
			if err != nil {
				t.Fatalf("toolbroker.New: %v", err)
			}
			registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				aRuns.Add(1)
				return toolbroker.ToolResult{}, errors.New("boom")
			})
			registerBudgetTestTool(t, broker, "tool_b", &approval.Rule{
				ToolName:         "tool_b",
				ToolVersion:      tools.Version,
				RequiresApproval: true,
				AllowedTrust:     []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustTrustedConfiguration, approval.TrustDerivedUntrusted},
			}, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				bRuns.Add(1)
				return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			})

			model := &budgetScriptModel{steps: []budgetScriptStep{
				{calls: []string{"tool_a"}},
				{calls: []string{"tool_a"}},
				{calls: []string{"tool_a"}},
				{calls: []string{fourthTool}},
				{text: "I tried tool_a three times and it kept failing."},
			}}
			logs := &logCaptureHandler{}
			cfg := runtimeengine.Default()
			events, db := runDurableBudgetTurn(t, model, broker, cfg, slog.New(logs), nil, nil, ownerTurn("turn-1"))

			if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
				t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
			}
			if got := aRuns.Load(); got != 3 {
				t.Errorf("tool_a runs = %d, want 3", got)
			}
			if got := bRuns.Load(); got != 0 {
				t.Errorf("tool_b runs = %d, want 0", got)
			}
			if got := approvalPrompts.Load(); got != 0 {
				t.Errorf("approval prompts = %d, want 0", got)
			}
			keys := dbActionKeys(t, db, "turn-1")
			if slices.Contains(keys, "call-4-0/policy") {
				t.Errorf("keys = %v, want no call-4-0/policy", keys)
			}
			if slices.Contains(keys, "call-4-0/authorization") || slices.Contains(keys, "call-4-0/execution") {
				t.Errorf("keys = %v, want no authorization or execution for refused call", keys)
			}
			if raw := dbActionResult(t, db, "turn-1", "call-4-0/budget"); raw != `{"allowed":false,"reason":"streak"}` {
				t.Errorf("call-4-0/budget = %s, want streak refusal", raw)
			}
			reqs := model.requests()
			if len(reqs) != 5 {
				t.Fatalf("model calls = %d, want 5", len(reqs))
			}
			out, ok := lastFunctionResponseOutput(reqs[4], fourthTool)
			if !ok || !strings.Contains(out, wantRefusal) {
				t.Errorf("4th response = %s, want %q", out, wantRefusal)
			}
			exhaustedLogs := logs.matching("tool budget exhausted")
			if len(exhaustedLogs) != 1 {
				t.Fatalf("exhaustion log count = %d, want 1", len(exhaustedLogs))
			}
			if exhaustedLogs[0].level != slog.LevelInfo || exhaustedLogs[0].attrs["turn_id"] != "turn-1" || exhaustedLogs[0].attrs["reason"] != "streak" {
				t.Errorf("exhaustion log = %+v, want info turn_id=turn-1 reason=streak", exhaustedLogs[0])
			}
		})
	}

	t.Run("success resets streak", func(t *testing.T) {
		var runs atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			n := runs.Add(1)
			if n == 3 {
				return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			}
			return toolbroker.ToolResult{}, errors.New("transient error")
		})
		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{text: "done after reset"},
		}}
		cfg := runtimeengine.Default()
		events, _ := runDurableBudgetTurn(t, model, broker, cfg, nil, nil, nil, ownerTurn("turn-reset"))
		if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
		}
		if got := runs.Load(); got != 6 {
			t.Errorf("tool_a runs = %d, want 6 (2 fail, 1 ok resets, 3 fail, 7th refused)", got)
		}
		reqs := model.requests()
		if len(reqs) != 8 {
			t.Fatalf("model calls = %d, want 8", len(reqs))
		}
		if out, ok := lastFunctionResponseOutput(reqs[7], "tool_a"); !ok || !strings.Contains(out, wantRefusal) {
			t.Errorf("7th response = %s, want %q", out, wantRefusal)
		}
	})

	t.Run("different tools failing once each do not form a streak", func(t *testing.T) {
		var runs atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		for _, name := range []string{"tool_a", "tool_b", "tool_c"} {
			registerBudgetTestTool(t, broker, name, nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				runs.Add(1)
				return toolbroker.ToolResult{}, errors.New("tool error")
			})
		}
		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_b"}},
			{calls: []string{"tool_c"}},
			{calls: []string{"tool_a"}},
			{text: "all four ran"},
		}}
		cfg := runtimeengine.Default()
		events, _ := runDurableBudgetTurn(t, model, broker, cfg, nil, nil, nil, ownerTurn("turn-mixed"))
		if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
		}
		if got := runs.Load(); got != 4 {
			t.Errorf("tool runs = %d, want 4 (no streak across different tools)", got)
		}
	})
}

func TestBudgetRefusesAfterTotalErrors(t *testing.T) {
	wantRefusal := "tool_budget_exhausted: 8 tool calls did not succeed in this turn. Do not call any more tools in this turn. Reply to the user now, in their language: what you tried, why it did not succeed, and what they can do next."

	var rejectedApprovals atomic.Int64
	var adapterRuns atomic.Int64
	broker, err := toolbroker.New(&toolbroker.Options{
		HostCapabilities: capabilitytest.Host(t),
		ApprovalDecider: func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) {
			rejectedApprovals.Add(1)
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	failAdapter := func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		adapterRuns.Add(1)
		return toolbroker.ToolResult{}, errors.New("failed")
	}
	registerBudgetTestTool(t, broker, "tool_a", nil, failAdapter)
	registerBudgetTestTool(t, broker, "tool_approval", &approval.Rule{
		ToolName:         "tool_approval",
		ToolVersion:      tools.Version,
		RequiresApproval: true,
		AllowedTrust:     []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustTrustedConfiguration, approval.TrustDerivedUntrusted},
	}, failAdapter)
	registerBudgetTestTool(t, broker, "tool_denied", &approval.Rule{
		ToolName:     "tool_denied",
		ToolVersion:  tools.Version,
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput},
	}, failAdapter)
	registerBudgetTestTool(t, broker, "tool_d", nil, failAdapter)

	now := time.Now().UTC()
	var clockMu sync.Mutex
	currentTime := now
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return currentTime
	}

	model := &budgetScriptModel{
		steps: []budgetScriptStep{
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_approval"}},
			{calls: []string{"tool_approval"}},
			{calls: []string{"tool_denied"}},
			{calls: []string{"tool_denied"}},
			{calls: []string{"tool_d"}},
			{calls: []string{"tool_d"}},
			{calls: []string{"tool_a", "tool_d"}},
			{text: "stopped after 8 errors"},
		},
	}

	cfg := runtimeengine.Default()
	events, db := runDurableBudgetTurn(t, model, broker, cfg, nil, []ExecutorOption{WithClock(clock)}, nil, ownerTurn("turn-total"))
	if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
		t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
	}
	if got := rejectedApprovals.Load(); got != 2 {
		t.Errorf("rejected approvals = %d, want 2", got)
	}
	if got := adapterRuns.Load(); got != 4 {
		t.Errorf("adapter runs = %d, want 4 (2 tool_a + 2 tool_d)", got)
	}
	for _, key := range []string{"call-9-0/budget", "call-9-1/budget"} {
		if raw := dbActionResult(t, db, "turn-total", key); raw != `{"allowed":false,"reason":"total"}` {
			t.Errorf("%s = %s, want total refusal", key, raw)
		}
	}
	reqs := model.requests()
	if len(reqs) != 10 {
		t.Fatalf("model calls = %d, want 10", len(reqs))
	}
	responses := allFunctionResponses(reqs[9])
	if len(responses) < 2 {
		t.Fatalf("responses = %d, want at least 2", len(responses))
	}
	for _, part := range responses[len(responses)-2:] {
		if !strings.Contains(part.output, wantRefusal) {
			t.Errorf("refused response for %s = %s, want %q", part.responseName, part.output, wantRefusal)
		}
	}

	t.Run("refused calls are not counted and first reason is kept", func(t *testing.T) {
		clockNow := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		ctx, cancel := context.WithDeadline(t.Context(), clockNow.Add(5*time.Minute))
		defer cancel()
		budget := newTurnToolBudget(
			ctx,
			ToolBudget{AnswerReserve: 30 * time.Second, MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8},
			"turn-first-reason",
			func() time.Time { return clockNow },
			slog.New(slog.DiscardHandler),
		)
		for _, toolName := range []string{"tool_a", "tool_a", "tool_b", "tool_b", "tool_c", "tool_c", "tool_d", "tool_d"} {
			budget.recordToolResponse(toolName, true)
		}
		first, err := budget.check(ctx, nil, "tool_a", "")
		if err != nil || first.allowed {
			t.Fatalf("first check = %+v err=%v, want refusal", first, err)
		}
		if got := first.refusal["error"]; got != wantRefusal {
			t.Fatalf("first refusal = %v, want %q", got, wantRefusal)
		}
		if err := budget.observeEvent(&session.Event{
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{
					Role:  genai.RoleUser,
					Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "tool_a", Response: first.refusal}}},
				},
			},
		}, turn.Origin("discord:home")); err != nil {
			t.Fatalf("observeEvent: %v", err)
		}
		if budget.totalErrors != 8 {
			t.Fatalf("totalErrors after observing refusal = %d, want 8", budget.totalErrors)
		}
		if budget.streaks["tool_a"] != 2 {
			t.Fatalf("streak[tool_a] after observing refusal = %d, want 2", budget.streaks["tool_a"])
		}
		clockNow = clockNow.Add(4*time.Minute + 45*time.Second)
		second, err := budget.check(ctx, nil, "tool_a", "")
		if err != nil || second.allowed {
			t.Fatalf("second check = %+v err=%v, want refusal", second, err)
		}
		if got := second.refusal["error"]; got != wantRefusal {
			t.Fatalf("second refusal = %v, want first reason %q kept", got, wantRefusal)
		}
	})
}

func TestBudgetRefusesInsideTheAnswerReserve(t *testing.T) {
	wantRefusal := "tool_budget_exhausted: time for this turn is almost up. Do not call any more tools in this turn. Reply to the user now, in their language: what you tried, why it did not succeed, and what they can do next."

	cases := []struct {
		name            string
		turnTimeout     time.Duration
		allowedAt       time.Duration
		refusedAt       time.Duration
		expectedReserve time.Duration
	}{
		{name: "default 30s reserve on 5m turn", turnTimeout: 5 * time.Minute, allowedAt: 4*time.Minute + 29*time.Second, refusedAt: 4*time.Minute + 30*time.Second, expectedReserve: 30 * time.Second},
		{name: "half-turn 20s reserve on 40s turn", turnTimeout: 40 * time.Second, allowedAt: 19 * time.Second, refusedAt: 20 * time.Second, expectedReserve: 20 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Now().UTC()
			var clockMu sync.Mutex
			now := base
			setClock := func(at time.Time) {
				clockMu.Lock()
				now = at
				clockMu.Unlock()
			}
			clock := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}

			var runs atomic.Int64
			broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
			if err != nil {
				t.Fatalf("toolbroker.New: %v", err)
			}
			registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				runs.Add(1)
				return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			})
			builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
			if err != nil {
				t.Fatalf("NewBrokerExecutor: %v", err)
			}

			model := &budgetScriptModel{
				steps: []budgetScriptStep{
					{calls: []string{"tool_a"}},
					{calls: []string{"tool_a"}},
					{text: "answering in reserve"},
				},
				onStep: func(step int, _ requestParts) {
					switch step {
					case 0:
						setClock(base.Add(tc.allowedAt))
					case 1:
						setClock(base.Add(tc.refusedAt))
					}
				},
			}

			db, sessions, _ := newSessionTestDB(t)
			mustCreateSession(t, db, "session-1")
			executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
				WithBuiltinToolExecutor(builtin),
				WithToolBudget(ToolBudget{AnswerReserve: 30 * time.Second, MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8}),
				WithClock(clock),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			ctx, cancel := context.WithDeadline(t.Context(), base.Add(tc.turnTimeout))
			defer cancel()
			for _, err := range executor.Execute(ctx, ownerTurn("turn-reserve")) {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			}
			if got := runs.Load(); got != 1 {
				t.Errorf("tool runs = %d, want 1 (first before D-%v, second at D-%v refused)", got, tc.expectedReserve, tc.expectedReserve)
			}
			reqs := model.requests()
			if len(reqs) != 3 {
				t.Fatalf("model calls = %d, want 3", len(reqs))
			}
			if out, ok := lastFunctionResponseOutput(reqs[2], "tool_a"); !ok || !strings.Contains(out, wantRefusal) {
				t.Errorf("second response = %s, want %q", out, wantRefusal)
			}
		})
	}
}

func TestRunningCallIsCutAtTheSoftDeadline(t *testing.T) {
	t.Run("blocking tool cut at soft deadline", func(t *testing.T) {
		var toolObservedDeadline time.Time
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "slow_tool", nil, func(ctx context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			toolObservedDeadline = req.Deadline
			<-ctx.Done()
			return toolbroker.ToolResult{}, ctx.Err()
		})

		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"slow_tool"}},
			{text: "The tool timed out before the turn ended."},
		}}
		ch := &captureChannel{name: "discord"}
		cfg := runtimeengine.Default()
		cfg.TurnTimeout = configkit.Duration(900 * time.Millisecond)
		cfg.AnswerReserve = configkit.Duration(450 * time.Millisecond)

		started := time.Now()
		events, db := runDurableBudgetTurn(t, model, broker, cfg, nil, nil, []runtimechannelhost.ChannelPort{ch}, channelTurnRequest("turn-cut-tool"))
		if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
		}
		if toolObservedDeadline.IsZero() {
			t.Fatal("tool did not receive soft deadline")
		}
		var turnStart struct {
			StartedAt time.Time `json:"started_at"`
		}
		if err := json.Unmarshal([]byte(dbActionResult(t, db, "turn-cut-tool", "turn-start")), &turnStart); err != nil {
			t.Fatalf("decode turn-start: %v", err)
		}
		hardDeadline := turnStart.StartedAt.Add(900 * time.Millisecond)
		recordedBudget, ok := decodeBudgetDecision([]byte(dbActionResult(t, db, "turn-cut-tool", "call-1-0/budget")))
		if !ok || !recordedBudget.Allowed || recordedBudget.SoftDeadline == nil {
			t.Fatalf("call-1-0/budget = %s, want allowed soft_deadline", dbActionResult(t, db, "turn-cut-tool", "call-1-0/budget"))
		}
		if !toolObservedDeadline.Equal(*recordedBudget.SoftDeadline) || !toolObservedDeadline.Before(hardDeadline) {
			t.Errorf("tool deadline = %v, want recorded soft deadline %v strictly before hard deadline %v", toolObservedDeadline, *recordedBudget.SoftDeadline, hardDeadline)
		}
		if elapsed := time.Since(started); elapsed >= 900*time.Millisecond {
			t.Errorf("turn elapsed %v, expected completion before hard deadline", elapsed)
		}
		reqs := model.requests()
		if len(reqs) != 2 {
			t.Fatalf("model calls = %d, want 2", len(reqs))
		}
		if out, ok := lastFunctionResponseOutput(reqs[1], "slow_tool"); !ok || !strings.Contains(out, string(toolbroker.ResultDeadlineExceeded)) {
			t.Errorf("tool response = %s, want deadline_exceeded", out)
		}
		if texts := ch.texts(); !slices.Equal(texts, []string{"The tool timed out before the turn ended."}) {
			t.Errorf("delivered texts = %q, want model text answer", texts)
		}
	})

	t.Run("blocking approval wait cut at soft deadline", func(t *testing.T) {
		var approvalSawCancel atomic.Bool
		var approvalExpiresAt time.Time
		var approvalMu sync.Mutex
		broker, err := toolbroker.New(&toolbroker.Options{
			HostCapabilities: capabilitytest.Host(t),
			ApprovalDecider: func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
				approvalMu.Lock()
				approvalExpiresAt = prompt.ExpiresAt
				approvalMu.Unlock()
				<-ctx.Done()
				approvalSawCancel.Store(true)
				return false, ctx.Err()
			},
		})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "gated_tool", &approval.Rule{
			ToolName:         "gated_tool",
			ToolVersion:      tools.Version,
			RequiresApproval: true,
			AllowedTrust:     []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustTrustedConfiguration, approval.TrustDerivedUntrusted},
		}, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})

		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"gated_tool"}},
			{text: "Approval did not arrive in time."},
		}}
		ch := &captureChannel{name: "discord"}
		cfg := runtimeengine.Default()
		cfg.TurnTimeout = configkit.Duration(900 * time.Millisecond)
		cfg.AnswerReserve = configkit.Duration(450 * time.Millisecond)

		events, db := runDurableBudgetTurn(t, model, broker, cfg, nil, nil, []runtimechannelhost.ChannelPort{ch}, channelTurnRequest("turn-cut-approval"))
		if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
		}
		if !approvalSawCancel.Load() {
			t.Fatal("approval wait was not cancelled at soft deadline")
		}
		var turnStart struct {
			StartedAt time.Time `json:"started_at"`
		}
		if err := json.Unmarshal([]byte(dbActionResult(t, db, "turn-cut-approval", "turn-start")), &turnStart); err != nil {
			t.Fatalf("decode turn-start: %v", err)
		}
		hardDeadline := turnStart.StartedAt.Add(900 * time.Millisecond)
		recordedBudget, ok := decodeBudgetDecision([]byte(dbActionResult(t, db, "turn-cut-approval", "call-1-0/budget")))
		if !ok || !recordedBudget.Allowed || recordedBudget.SoftDeadline == nil {
			t.Fatalf("call-1-0/budget = %s, want allowed soft_deadline", dbActionResult(t, db, "turn-cut-approval", "call-1-0/budget"))
		}
		approvalMu.Lock()
		gotExpiry := approvalExpiresAt
		approvalMu.Unlock()
		if !gotExpiry.Equal(*recordedBudget.SoftDeadline) || !gotExpiry.Before(hardDeadline) {
			t.Errorf("approval ExpiresAt = %v, want recorded soft deadline %v strictly before hard deadline %v", gotExpiry, *recordedBudget.SoftDeadline, hardDeadline)
		}
		reqs := model.requests()
		if len(reqs) != 2 {
			t.Fatalf("model calls = %d, want 2", len(reqs))
		}
		if out, ok := lastFunctionResponseOutput(reqs[1], "gated_tool"); !ok || !strings.Contains(out, string(toolbroker.ResultDeadlineExceeded)) {
			t.Errorf("gated_tool response = %s, want deadline_exceeded", out)
		}
		if texts := ch.texts(); !slices.Equal(texts, []string{"Approval did not arrive in time."}) {
			t.Errorf("delivered texts = %q, want model text answer", texts)
		}
	})
}

func TestModelAnswersAfterRefusal(t *testing.T) {
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{}, errors.New("broken")
	})
	model := &budgetScriptModel{steps: []budgetScriptStep{
		{calls: []string{"tool_a"}},
		{calls: []string{"tool_a"}},
		{calls: []string{"tool_a"}},
		{calls: []string{"tool_a"}},
		{text: "I tried tool_a three times, it failed, please verify credentials."},
	}}
	ch := &captureChannel{name: "discord"}
	cfg := runtimeengine.Default()
	events, _ := runDurableBudgetTurn(t, model, broker, cfg, nil, nil, []runtimechannelhost.ChannelPort{ch}, channelTurnRequest("turn-answer-after-refusal"))
	if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
		t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
	}
	if texts := ch.texts(); !slices.Equal(texts, []string{"I tried tool_a three times, it failed, please verify credentials."}) {
		t.Errorf("delivered texts = %q, want model explanation", texts)
	}
}

func TestToolRequestAfterARefusedBatchEndsTheTurn(t *testing.T) {
	var runs atomic.Int64
	broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	for _, name := range []string{"tool_a", "tool_b", "tool_c"} {
		registerBudgetTestTool(t, broker, name, nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			runs.Add(1)
			return toolbroker.ToolResult{}, errors.New("still failing")
		})
	}

	model := &budgetScriptModel{steps: []budgetScriptStep{
		{calls: []string{"tool_a"}},
		{calls: []string{"tool_a"}},
		{calls: []string{"tool_a"}},
		{calls: []string{"tool_a", "tool_b", "tool_c"}},
		{calls: []string{"tool_a"}},
		{text: "should never reach this step"},
	}}
	ch := &captureChannel{name: "discord"}
	cfg := runtimeengine.Default()
	events, _ := runDurableBudgetTurn(t, model, broker, cfg, nil, nil, []runtimechannelhost.ChannelPort{ch}, channelTurnRequest("turn-hard-bound"))

	code, _ := decodeTerminalFailure(t, events)
	if code != turn.ErrorCodeToolBudgetExhausted {
		t.Fatalf("terminal code = %q, want %q", code, turn.ErrorCodeToolBudgetExhausted)
	}
	if got := runs.Load(); got != 3 {
		t.Errorf("tool runs = %d, want 3 (batch 4 refused, batch 5 never executed)", got)
	}
	reqs := model.requests()
	if len(reqs) != 5 {
		t.Fatalf("model calls = %d, want 5", len(reqs))
	}
	responses := allFunctionResponses(reqs[4])
	if len(responses) < 3 {
		t.Fatalf("responses before 5th model call = %d, want at least 3", len(responses))
	}
	for _, part := range responses[len(responses)-3:] {
		if !strings.Contains(part.output, "tool_budget_exhausted:") {
			t.Errorf("parallel batch response for %s = %s, want refusal", part.responseName, part.output)
		}
	}
	wantFallback := auraagent.PersonaMessageDefaults()[auraagent.MessageTurnFailed]
	if texts := ch.texts(); !slices.Equal(texts, []string{wantFallback}) {
		t.Errorf("delivered fallback = %q, want %q", texts, wantFallback)
	}
}

func TestHandoffIsNeverRefused(t *testing.T) {
	t.Run("allowed after streak exhaustion", func(t *testing.T) {
		var handoffRuns atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			return toolbroker.ToolResult{}, errors.New("fail")
		})
		registerBudgetTestTool(t, broker, tools.HandoffToolName, nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			handoffRuns.Add(1)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"handed_off":true}`)}, nil
		})
		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{"tool_a"}},
			{calls: []string{tools.HandoffToolName}},
			{text: "handed off"},
		}}
		events, _ := runDurableBudgetTurn(t, model, broker, runtimeengine.Default(), nil, nil, nil, ownerTurn("turn-handoff-streak"))
		if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
		}
		if got := handoffRuns.Load(); got != 1 {
			t.Fatalf("handoff runs = %d, want 1", got)
		}
	})

	t.Run("allowed inside answer reserve and after a refused batch", func(t *testing.T) {
		base := time.Now().UTC()
		var clockMu sync.Mutex
		now := base
		setClock := func(at time.Time) {
			clockMu.Lock()
			now = at
			clockMu.Unlock()
		}
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}

		var toolARuns atomic.Int64
		var handoffRuns atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			toolARuns.Add(1)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})
		registerBudgetTestTool(t, broker, tools.HandoffToolName, nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			handoffRuns.Add(1)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"handed_off":true}`)}, nil
		})
		builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
		if err != nil {
			t.Fatalf("NewBrokerExecutor: %v", err)
		}

		model := &budgetScriptModel{
			steps: []budgetScriptStep{
				{calls: []string{"tool_a"}},
				{calls: []string{tools.HandoffToolName}},
				{text: "handed off in reserve"},
			},
			onStep: func(step int, _ requestParts) {
				if step == 0 {
					setClock(base.Add(4*time.Minute + 45*time.Second))
				}
			},
		}
		db, sessions, _ := newSessionTestDB(t)
		mustCreateSession(t, db, "session-1")
		executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
			WithBuiltinToolExecutor(builtin),
			WithToolBudget(ToolBudget{AnswerReserve: 30 * time.Second, MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8}),
			WithClock(clock),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		ctx, cancel := context.WithDeadline(t.Context(), base.Add(5*time.Minute))
		defer cancel()
		for _, err := range executor.Execute(ctx, ownerTurn("turn-handoff-reserve")) {
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
		}
		if got := toolARuns.Load(); got != 0 {
			t.Fatalf("tool_a runs = %d, want 0 (refused inside answer reserve)", got)
		}
		if got := handoffRuns.Load(); got != 1 {
			t.Fatalf("handoff runs = %d, want 1", got)
		}
		reqs := model.requests()
		if len(reqs) != 3 {
			t.Fatalf("model calls = %d, want 3", len(reqs))
		}
		if out, ok := lastFunctionResponseOutput(reqs[1], "tool_a"); !ok || !strings.Contains(out, "tool_budget_exhausted: time for this turn is almost up") {
			t.Errorf("tool_a response = %s, want time exhaustion refusal", out)
		}
	})
}

func TestBudgetDecisionIsJournaled(t *testing.T) {
	t.Run("replay with clock past soft deadline re-allows under recorded soft deadline", func(t *testing.T) {
		db, sessions, events := newSessionTestDB(t)
		mustCreateSession(t, db, "session-1")

		base := time.Now().UTC()
		var clockMu sync.Mutex
		currentClock := base
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return currentClock
		}
		setClock := func(at time.Time) {
			clockMu.Lock()
			currentClock = at
			clockMu.Unlock()
		}

		var (
			toolMu        sync.Mutex
			runs          int
			life2Deadline time.Time
			stallEntered  = make(chan struct{})
			stallOnce     sync.Once
		)
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "tool_a", nil, func(ctx context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			toolMu.Lock()
			runs++
			attempt := runs
			if attempt == 2 {
				life2Deadline = req.Deadline
			}
			toolMu.Unlock()
			if attempt == 1 {
				stallOnce.Do(func() { close(stallEntered) })
				<-ctx.Done()
				return toolbroker.ToolResult{}, ctx.Err()
			}
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})
		builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
		if err != nil {
			t.Fatalf("NewBrokerExecutor: %v", err)
		}

		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"tool_a"}},
			{text: "completed on replay"},
		}}
		cfg := runtimeengine.Default()
		cfg.TurnTimeout = configkit.Duration(5 * time.Minute)
		cfg.AnswerReserve = configkit.Duration(30 * time.Second)

		startHost := func() (*runtimeengine.Engine, func()) {
			executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
				WithBuiltinToolExecutor(builtin),
				WithToolBudget(ToolBudget{AnswerReserve: 30 * time.Second, MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8}),
				WithClock(clock),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler), PollInterval: durableTurnPoll, HostTTL: durableTurnHostTTL})
			if err != nil {
				t.Fatalf("sqlite.New: %v", err)
			}
			sessionStore, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db, Turns: turnstore.NewTurnStore(db)})
			if err != nil {
				t.Fatalf("sqlite.NewSessionStore: %v", err)
			}
			eng, err := runtimeengine.New(&runtimeengine.Deps{
				Events:     events,
				Dedupe:     turnstore.NewDedupeStore(db),
				Executor:   executor,
				Logger:     slog.New(slog.DiscardHandler),
				Config:     cfg,
				Durable:    &runtimeengine.DurableConfig{Sessions: sessionStore, Runtime: rt},
				LiveEvents: turn.NewEventBus(nil),
			})
			if err != nil {
				t.Fatalf("runtimeengine.New: %v", err)
			}
			hostCtx, cancelHost := context.WithCancel(context.WithoutCancel(t.Context()))
			hostDone := make(chan error, 1)
			go func() { hostDone <- rt.Run(hostCtx) }()
			stop := sync.OnceFunc(func() {
				cancelHost()
				<-hostDone
			})
			t.Cleanup(func() {
				shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				_ = eng.Shutdown(shutdownCtx)
				stop()
			})
			return eng, stop
		}

		eng1, stop1 := startHost()
		go func() {
			for range eng1.Run(context.WithoutCancel(t.Context()), ownerTurn("turn-replay-budget")) {
			}
		}()

		select {
		case <-stallEntered:
		case <-time.After(durableTurnWait):
			t.Fatal("life 1 never reached tool execution")
		}
		waitFor(t, "budget and policy actions recorded", func() bool {
			keys := dbActionKeys(t, db, "turn-replay-budget")
			return slices.Contains(keys, "call-1-0/budget") && slices.Contains(keys, "call-1-0/policy")
		})
		keys := dbActionKeys(t, db, "turn-replay-budget")
		budgetIdx := slices.Index(keys, "call-1-0/budget")
		policyIdx := slices.Index(keys, "call-1-0/policy")
		if budgetIdx < 0 || policyIdx < 0 || budgetIdx >= policyIdx {
			t.Fatalf("action order = %v, want call-1-0/budget before call-1-0/policy", keys)
		}
		recordedRaw := dbActionResult(t, db, "turn-replay-budget", "call-1-0/budget")
		recorded, ok := decodeBudgetDecision([]byte(recordedRaw))
		if !ok || !recorded.Allowed || recorded.SoftDeadline == nil {
			t.Fatalf("recorded budget = %s, want allowed with soft_deadline", recordedRaw)
		}

		stop1()
		setClock(recorded.SoftDeadline.Add(45 * time.Second))
		_, _ = startHost()

		waitFor(t, "replayed turn ends", func() bool {
			return slices.Contains(dbActionKeys(t, db, "turn-replay-budget"), "release")
		})

		toolMu.Lock()
		gotRuns := runs
		gotDeadline := life2Deadline
		toolMu.Unlock()
		if gotRuns != 2 {
			t.Fatalf("tool runs = %d, want 2 (re-allowed on replay)", gotRuns)
		}
		if !gotDeadline.Equal(*recorded.SoftDeadline) {
			t.Fatalf("life 2 tool deadline = %v, want recorded soft_deadline %v", gotDeadline, *recorded.SoftDeadline)
		}
		var budgetCount int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key LIKE '%/budget'`, "turn-replay-budget").Scan(&budgetCount); err != nil {
			t.Fatalf("count budget rows: %v", err)
		}
		if budgetCount != 1 {
			t.Fatalf("budget row count = %d, want 1", budgetCount)
		}
		var storedEvents []turn.Event
		storedEvents, err = turnstore.NewDedupeStore(db).ListTurnEvents(t.Context(), "session-1", "turn-replay-budget")
		if err != nil {
			t.Fatalf("ListTurnEvents: %v", err)
		}
		if len(storedEvents) == 0 || storedEvents[len(storedEvents)-1].Kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal event = %+v, want turn.completed", storedEvents)
		}
	})

	t.Run("journal version 3 turn-start fails before executor", func(t *testing.T) {
		db, sessions, events := newSessionTestDB(t)
		mustCreateSession(t, db, "session-1")
		model := &budgetScriptModel{steps: []budgetScriptStep{{text: "should not run"}}}
		executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, &fakeBroker{}, nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler), PollInterval: durableTurnPoll, HostTTL: durableTurnHostTTL})
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		sessionStore, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db, Turns: turnstore.NewTurnStore(db)})
		if err != nil {
			t.Fatalf("sqlite.NewSessionStore: %v", err)
		}
		eng, err := runtimeengine.New(&runtimeengine.Deps{
			Events:     events,
			Dedupe:     turnstore.NewDedupeStore(db),
			Executor:   executor,
			Logger:     slog.New(slog.DiscardHandler),
			Config:     runtimeengine.Default(),
			Durable:    &runtimeengine.DurableConfig{Sessions: sessionStore, Runtime: rt},
			LiveEvents: turn.NewEventBus(nil),
		})
		if err != nil {
			t.Fatalf("runtimeengine.New: %v", err)
		}

		streamDone := make(chan []turn.Event, 1)
		go func() {
			var got []turn.Event
			for ev, err := range eng.Run(context.WithoutCancel(t.Context()), ownerTurn("turn-v3")) {
				if err != nil {
					t.Errorf("engine.Run: %v", err)
					break
				}
				got = append(got, ev)
			}
			streamDone <- got
		}()
		waitFor(t, "turn-v3 run exists", func() bool {
			var n int
			_ = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_run WHERE key = ?`, "turn-v3").Scan(&n)
			return n == 1
		})
		v3 := `{"started_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","journal":3}`
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, detail, created_at) VALUES (?, 'turn-start', 'result', ?, '', ?)`, "turn-v3", []byte(v3), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed turn-start v3: %v", err)
		}
		hostCtx, cancelHost := context.WithCancel(context.WithoutCancel(t.Context()))
		hostDone := make(chan error, 1)
		go func() { hostDone <- rt.Run(hostCtx) }()
		t.Cleanup(func() {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
			defer cancel()
			_ = eng.Shutdown(shutdownCtx)
			cancelHost()
			<-hostDone
		})

		gotEvents := <-streamDone
		code, detail := decodeTerminalFailure(t, gotEvents)
		if code != turn.ErrorCodeRuntimeInternal || !strings.Contains(detail, "turn journal was written by another build") {
			t.Fatalf("terminal = %q %q, want runtime_internal from another build", code, detail)
		}
		if model.calls() != 0 {
			t.Fatalf("model calls = %d, want 0", model.calls())
		}
	})

	t.Run("real wall-clock replay inside answer reserve replays succeeded call and refuses next call", func(t *testing.T) {
		db, sessions, events := newSessionTestDB(t)
		mustCreateSession(t, db, "session-1")

		var toolRuns atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "tool_a", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			toolRuns.Add(1)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})
		builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
		if err != nil {
			t.Fatalf("NewBrokerExecutor: %v", err)
		}

		model2Entered := make(chan struct{})
		var model2Once sync.Once
		var life atomic.Int32
		life.Store(1)

		var model2SeenInLife2 requestParts
		var model3SeenInLife2 requestParts
		var seenMu sync.Mutex

		model := &budgetScriptModel{
			steps: []budgetScriptStep{
				{calls: []string{"tool_a"}},
				{calls: []string{"tool_a"}},
				{text: "replayed cleanly in reserve"},
			},
			onCall: func(ctx context.Context, step int, seen requestParts) error {
				if step == 1 && life.Load() == 1 {
					model2Once.Do(func() { close(model2Entered) })
					<-ctx.Done()
					return ctx.Err()
				}
				if step == 1 {
					seenMu.Lock()
					model2SeenInLife2 = seen
					seenMu.Unlock()
				}
				if step == 2 {
					seenMu.Lock()
					model3SeenInLife2 = seen
					seenMu.Unlock()
				}
				return nil
			},
		}

		cfg := runtimeengine.Default()
		cfg.TurnTimeout = configkit.Duration(2 * time.Second)
		cfg.AnswerReserve = configkit.Duration(1 * time.Second)

		startHost := func() (*runtimeengine.Engine, func()) {
			executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
				WithBuiltinToolExecutor(builtin),
				WithToolBudget(ToolBudget{AnswerReserve: time.Duration(cfg.AnswerReserve), MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8}),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler), PollInterval: durableTurnPoll, HostTTL: durableTurnHostTTL})
			if err != nil {
				t.Fatalf("sqlite.New: %v", err)
			}
			sessionStore, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db, Turns: turnstore.NewTurnStore(db)})
			if err != nil {
				t.Fatalf("sqlite.NewSessionStore: %v", err)
			}
			eng, err := runtimeengine.New(&runtimeengine.Deps{
				Events:     events,
				Dedupe:     turnstore.NewDedupeStore(db),
				Executor:   executor,
				Logger:     slog.New(slog.DiscardHandler),
				Config:     cfg,
				Durable:    &runtimeengine.DurableConfig{Sessions: sessionStore, Runtime: rt},
				LiveEvents: turn.NewEventBus(nil),
			})
			if err != nil {
				t.Fatalf("runtimeengine.New: %v", err)
			}
			hostCtx, cancelHost := context.WithCancel(context.WithoutCancel(t.Context()))
			hostDone := make(chan error, 1)
			go func() { hostDone <- rt.Run(hostCtx) }()
			stop := sync.OnceFunc(func() {
				cancelHost()
				<-hostDone
			})
			t.Cleanup(func() {
				shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				_ = eng.Shutdown(shutdownCtx)
				stop()
			})
			return eng, stop
		}

		eng1, stop1 := startHost()
		go func() {
			for range eng1.Run(context.WithoutCancel(t.Context()), ownerTurn("turn-reserve-replay")) {
			}
		}()

		select {
		case <-model2Entered:
		case <-time.After(durableTurnWait):
			t.Fatal("life 1 never reached model-2")
		}
		stop1()

		recordedBudget, ok := decodeBudgetDecision([]byte(dbActionResult(t, db, "turn-reserve-replay", "call-1-0/budget")))
		if !ok || !recordedBudget.Allowed || recordedBudget.SoftDeadline == nil {
			t.Fatalf("call-1-0/budget = %s, want allowed soft_deadline", dbActionResult(t, db, "turn-reserve-replay", "call-1-0/budget"))
		}
		if wait := time.Until(recordedBudget.SoftDeadline.Add(50 * time.Millisecond)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-t.Context().Done():
				timer.Stop()
				t.Fatal(t.Context().Err())
			}
		}

		life.Store(2)
		model.mu.Lock()
		model.callIdx = 1
		model.mu.Unlock()
		_, _ = startHost()

		waitFor(t, "replayed turn completes in answer reserve", func() bool {
			return slices.Contains(dbActionKeys(t, db, "turn-reserve-replay"), "release")
		})

		if got := toolRuns.Load(); got != 1 {
			t.Fatalf("tool_a runs = %d, want 1 (call-1-0 replayed from execution journal, call-2-0 refused)", got)
		}
		if raw := dbActionResult(t, db, "turn-reserve-replay", "call-2-0/budget"); raw != `{"allowed":false,"reason":"time"}` {
			t.Fatalf("call-2-0/budget = %s, want time refusal", raw)
		}
		seenMu.Lock()
		gotM2, gotM3 := model2SeenInLife2, model3SeenInLife2
		seenMu.Unlock()
		if out, ok := lastFunctionResponseOutput(gotM2, "tool_a"); !ok || !strings.Contains(out, `"ok":true`) {
			t.Errorf("call-1-0 replayed response in model-2 = %s, want {\"ok\":true}", out)
		}
		if out, ok := lastFunctionResponseOutput(gotM3, "tool_a"); !ok || !strings.Contains(out, "tool_budget_exhausted: time for this turn is almost up") {
			t.Errorf("call-2-0 response in model-3 = %s, want time exhaustion refusal", out)
		}
		storedEvents, err := turnstore.NewDedupeStore(db).ListTurnEvents(t.Context(), "session-1", "turn-reserve-replay")
		if err != nil {
			t.Fatalf("ListTurnEvents: %v", err)
		}
		if len(storedEvents) == 0 || storedEvents[len(storedEvents)-1].Kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal event = %+v, want turn.completed", storedEvents)
		}
	})

	t.Run("replay of approval wait timed out at soft deadline preserves deadline_exceeded", func(t *testing.T) {
		db, sessions, events := newSessionTestDB(t)
		mustCreateSession(t, db, "session-1")

		var deciderCalls atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{
			HostCapabilities: capabilitytest.Host(t),
			ApprovalDecider: func(ctx context.Context, _ *toolbroker.ApprovalPrompt) (bool, error) {
				deciderCalls.Add(1)
				<-ctx.Done()
				return false, ctx.Err()
			},
		})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "gated_tool", &approval.Rule{
			ToolName:         "gated_tool",
			ToolVersion:      tools.Version,
			RequiresApproval: true,
			AllowedTrust:     []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustTrustedConfiguration, approval.TrustDerivedUntrusted},
		}, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})
		builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
		if err != nil {
			t.Fatalf("NewBrokerExecutor: %v", err)
		}

		model2Entered := make(chan struct{})
		var model2Once sync.Once
		var life atomic.Int32
		life.Store(1)

		var model2SeenInLife2 requestParts
		var seenMu sync.Mutex

		model := &budgetScriptModel{
			steps: []budgetScriptStep{
				{calls: []string{"gated_tool"}},
				{text: "approval timed out cleanly"},
			},
			onCall: func(ctx context.Context, step int, seen requestParts) error {
				if step == 1 && life.Load() == 1 {
					model2Once.Do(func() { close(model2Entered) })
					<-ctx.Done()
					return ctx.Err()
				}
				if step == 1 {
					seenMu.Lock()
					model2SeenInLife2 = seen
					seenMu.Unlock()
				}
				return nil
			},
		}

		cfg := runtimeengine.Default()
		cfg.TurnTimeout = configkit.Duration(2 * time.Second)
		cfg.AnswerReserve = configkit.Duration(1 * time.Second)

		startHost := func() (*runtimeengine.Engine, func()) {
			executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
				WithBuiltinToolExecutor(builtin),
				WithToolBudget(ToolBudget{AnswerReserve: time.Duration(cfg.AnswerReserve), MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8}),
			)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler), PollInterval: durableTurnPoll, HostTTL: durableTurnHostTTL})
			if err != nil {
				t.Fatalf("sqlite.New: %v", err)
			}
			sessionStore, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db, Turns: turnstore.NewTurnStore(db)})
			if err != nil {
				t.Fatalf("sqlite.NewSessionStore: %v", err)
			}
			eng, err := runtimeengine.New(&runtimeengine.Deps{
				Events:     events,
				Dedupe:     turnstore.NewDedupeStore(db),
				Executor:   executor,
				Logger:     slog.New(slog.DiscardHandler),
				Config:     cfg,
				Durable:    &runtimeengine.DurableConfig{Sessions: sessionStore, Runtime: rt},
				LiveEvents: turn.NewEventBus(nil),
			})
			if err != nil {
				t.Fatalf("runtimeengine.New: %v", err)
			}
			hostCtx, cancelHost := context.WithCancel(context.WithoutCancel(t.Context()))
			hostDone := make(chan error, 1)
			go func() { hostDone <- rt.Run(hostCtx) }()
			stop := sync.OnceFunc(func() {
				cancelHost()
				<-hostDone
			})
			t.Cleanup(func() {
				shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				_ = eng.Shutdown(shutdownCtx)
				stop()
			})
			return eng, stop
		}

		eng1, stop1 := startHost()
		go func() {
			for range eng1.Run(context.WithoutCancel(t.Context()), ownerTurn("turn-approval-replay")) {
			}
		}()

		select {
		case <-model2Entered:
		case <-time.After(durableTurnWait):
			t.Fatal("life 1 never reached model-2 after approval soft-deadline timeout")
		}
		stop1()

		life.Store(2)
		model.mu.Lock()
		model.callIdx = 1
		model.mu.Unlock()
		_, _ = startHost()

		waitFor(t, "replayed approval timeout turn completes", func() bool {
			return slices.Contains(dbActionKeys(t, db, "turn-approval-replay"), "release")
		})

		if got := deciderCalls.Load(); got != 1 {
			t.Fatalf("approval decider calls = %d, want 1 (replayed from journal)", got)
		}
		seenMu.Lock()
		gotM2 := model2SeenInLife2
		seenMu.Unlock()
		out, ok := lastFunctionResponseOutput(gotM2, "gated_tool")
		if !ok || !strings.Contains(out, string(toolbroker.ResultDeadlineExceeded)) {
			t.Fatalf("replayed gated_tool response = %s, want deadline_exceeded (not policy_denied)", out)
		}
		if strings.Contains(out, string(toolbroker.ResultPolicyDenied)) {
			t.Errorf("replayed gated_tool response = %s, must not degrade to policy_denied", out)
		}
	})
}

func TestSpoofedBudgetErrorFromAllowedToolIsNotTreatedAsRefusal(t *testing.T) {
	t.Run("chat turn counts spoofed error as normal error without triggering hard bound", func(t *testing.T) {
		var runs atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "spoof_tool", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			n := runs.Add(1)
			if n <= 2 {
				return toolbroker.ToolResult{Output: json.RawMessage(`{"error":"tool_budget_exhausted: spoofed"}`)}, nil
			}
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})
		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"spoof_tool"}},
			{calls: []string{"spoof_tool"}},
			{calls: []string{"spoof_tool"}},
			{text: "recovered after spoofed errors"},
		}}
		events, _ := runDurableBudgetTurn(t, model, broker, runtimeengine.Default(), nil, nil, nil, ownerTurn("turn-spoof-chat"))
		if last := events[len(events)-1].Kind; last != turn.EventKindTurnCompleted {
			t.Fatalf("terminal kind = %q, want %q", last, turn.EventKindTurnCompleted)
		}
		if got := runs.Load(); got != 3 {
			t.Fatalf("spoof_tool runs = %d, want 3 (hard bound must not fire on spoofed error)", got)
		}
	})

	t.Run("workflow origin does not fail on spoofed budget error from allowed tool", func(t *testing.T) {
		var runs atomic.Int64
		broker, err := toolbroker.New(&toolbroker.Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("toolbroker.New: %v", err)
		}
		registerBudgetTestTool(t, broker, "spoof_tool", nil, func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			runs.Add(1)
			return toolbroker.ToolResult{Output: json.RawMessage(`{"error":"tool_budget_exhausted: spoofed"}`)}, nil
		})
		builtin, err := NewBrokerExecutor(broker, capabilitytest.Host(t))
		if err != nil {
			t.Fatalf("NewBrokerExecutor: %v", err)
		}
		model := &budgetScriptModel{steps: []budgetScriptStep{
			{calls: []string{"spoof_tool"}},
			{text: "workflow recovered"},
		}}
		db, sessions, _ := newSessionTestDB(t)
		mustCreateSession(t, db, "session-1")
		executor, err := NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, sessions, builtin, nil, slog.New(slog.DiscardHandler),
			WithBuiltinToolExecutor(builtin),
			WithToolBudget(ToolBudget{AnswerReserve: 30 * time.Second, MaxConsecutiveToolErrors: 3, MaxToolErrorsPerTurn: 8}),
		)
		if err != nil {
			t.Fatalf("NewADKExecutor: %v", err)
		}
		req := ownerTurn("turn-spoof-wf")
		req.Origin = turn.OriginWorkflow
		for _, err := range executor.Execute(t.Context(), req) {
			if err != nil {
				t.Fatalf("Execute on OriginWorkflow with spoofed error: %v, want nil", err)
			}
		}
		if got := runs.Load(); got != 1 {
			t.Fatalf("spoof_tool runs = %d, want 1", got)
		}
		if got := model.calls(); got != 2 {
			t.Fatalf("model calls = %d, want 2", got)
		}
	})
}
