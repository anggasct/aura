//go:build durable

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	firstLifeEnv       = "AURA_DURABLE_TURN_FIRST_LIFE"
	suiteSession       = "session-1"
	suiteOwner         = "user-1"
	suiteDeadline      = 60 * time.Second
	suiteTick          = 20 * time.Millisecond
	suiteStopBound     = 10 * time.Second
	suiteHostTTL       = 2 * time.Second
	stubToolVersion    = "v1"
	modelCalls         = "model"
	scriptedModelName  = "scripted-model"
	scriptedAnswerText = "done"
	textStep           = ""
)

type lifePlan struct {
	Database string `json:"database"`
	Scenario string `json:"scenario"`
	Ledger   string `json:"ledger"`
	Life     int    `json:"life"`
}

type ledgerRecord struct {
	Life int    `json:"life"`
	Text string `json:"text"`
}

type turnLedger struct {
	dir  string
	life int

	mu sync.Mutex
}

func (l *turnLedger) path(name string) string {
	return filepath.Join(l.dir, name)
}

func (l *turnLedger) record(kind, text string) (position int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	records, err := l.read(kind)
	if err != nil {
		return 0, err
	}
	line, err := json.Marshal(ledgerRecord{Life: l.life, Text: text})
	if err != nil {
		return 0, fmt.Errorf("encode the %s record: %w", kind, err)
	}
	file, err := os.OpenFile(l.path(kind+".ledger"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open the %s ledger: %w", kind, err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return 0, fmt.Errorf("append to the %s ledger: %w", kind, err)
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("close the %s ledger: %w", kind, err)
	}
	return len(records), nil
}

func (l *turnLedger) read(kind string) ([]ledgerRecord, error) {
	raw, err := os.ReadFile(l.path(kind + ".ledger"))
	if errors.Is(err, fs.ErrNotExist) {
		return []ledgerRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the %s ledger: %w", kind, err)
	}
	records := []ledgerRecord{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		var rec ledgerRecord
		err := decoder.Decode(&rec)
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode the %s ledger: %w", kind, err)
		}
		records = append(records, rec)
	}
}

func (l *turnLedger) records(t *testing.T, kind string) []ledgerRecord {
	t.Helper()
	records, err := l.read(kind)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return records
}

func (l *turnLedger) requireCount(t *testing.T, kind string, want int) {
	t.Helper()
	if got := len(l.records(t, kind)); got != want {
		t.Errorf("%s records = %d, want %d", kind, got, want)
	}
}

func (l *turnLedger) exists(t *testing.T, name string) func() bool {
	t.Helper()
	return func() bool {
		_, err := os.Stat(l.path(name))
		if err == nil {
			return true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s: %v", name, err)
		}
		return false
	}
}

func (l *turnLedger) mark(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(l.path(name), []byte(name), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func (l *turnLedger) gate(ctx context.Context, tool string) error {
	if err := os.WriteFile(l.path(tool+".entered"), []byte("entered"), 0o600); err != nil {
		return fmt.Errorf("mark %s entered: %w", tool, err)
	}
	tick := time.NewTicker(suiteTick)
	defer tick.Stop()
	for {
		_, err := os.Stat(l.path(tool + ".release"))
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read the release of %s: %w", tool, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s waited for its release: %w", tool, ctx.Err())
		case <-tick.C:
		}
	}
}

func executions(tool string) string {
	return tool + ".exec"
}

var errStubToolFailed = errors.New("stub tool failed")

func (l *turnLedger) adapter(stub stubTool) toolbroker.Adapter {
	return func(ctx context.Context, _ *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if stub.gatedInAdapter {
			if err := l.gate(ctx, stub.name); err != nil {
				return toolbroker.ToolResult{}, err
			}
		}
		if _, err := l.record(executions(stub.name), ""); err != nil {
			return toolbroker.ToolResult{}, err
		}
		if stub.fails {
			return toolbroker.ToolResult{}, errStubToolFailed
		}
		return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
}

func (l *turnLedger) decider(approvingLife int) toolbroker.ApprovalDecider {
	return func(_ context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if _, err := l.record(approvalCalls, prompt.ToolName); err != nil {
			return false, err
		}
		return l.life == approvingLife, nil
	}
}

type scriptedModel struct {
	ledger *turnLedger
	steps  []string
}

func (m *scriptedModel) Name() string { return scriptedModelName }

func (m *scriptedModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		position, err := m.ledger.record(modelCalls, modelRequestText(req))
		if err != nil {
			yield(nil, err)
			return
		}
		part := &genai.Part{Text: scriptedAnswerText}
		if position < len(m.steps) && m.steps[position] != textStep {
			part = &genai.Part{FunctionCall: &genai.FunctionCall{
				ID:   fmt.Sprintf("call-%d", position+1),
				Name: m.steps[position],
				Args: map[string]any{"query": "x"},
			}}
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 1, CandidatesTokenCount: 1, TotalTokenCount: 2,
			},
		}, nil)
	}
}

func modelRequestText(req *adkmodel.LLMRequest) string {
	var seen strings.Builder
	if req == nil {
		return ""
	}
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			seen.WriteString(part.Text)
			seen.WriteString("\n")
		}
	}
	return seen.String()
}

type ledgerTools struct {
	broker *toolbroker.Broker
	ledger *turnLedger
	gated  string
	defs   []runtimeadk.BuiltinToolDefinition
}

func (l *ledgerTools) Definitions() []runtimeadk.BuiltinToolDefinition {
	return l.defs
}

func (l *ledgerTools) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return l.broker.Evaluate(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, RequestDigest: request.RequestDigest, Capabilities: request.Capabilities,
		Trust: request.Trust, Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
	})
}

func (l *ledgerTools) Execute(ctx context.Context, request *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	if request.ToolName == l.gated {
		if err := l.ledger.gate(ctx, request.ToolName); err != nil {
			return nil, err
		}
	}
	result, err := l.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID:             request.RequestID,
		TurnID:                request.TurnID,
		SessionID:             request.SessionID,
		PrincipalID:           request.PrincipalID,
		PrincipalClass:        request.PrincipalClass,
		AgentID:               request.AgentID,
		ToolName:              request.ToolName,
		ToolVersion:           request.ToolVersion,
		Arguments:             request.Arguments,
		Capabilities:          request.Capabilities,
		Trust:                 approval.TrustLabel(request.Trust),
		Deadline:              request.Deadline,
		IdempotencyKey:        request.IdempotencyKey,
		Knowledge:             request.Knowledge,
		Tainted:               request.Tainted,
		AllocateEventSequence: true,
		EventInvocation:       request.EventInvocation,
		EventBranch:           request.EventBranch,
		EventAuthor:           request.EventAuthor,
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

type turnLife struct {
	scenario turnScenario
	db       *sql.DB
	runtime  *sqlite.Runtime
	sessions *sqlite.SessionStore
	engine   *runtimeengine.Engine
	ledger   *turnLedger
}

func newSuiteRuntime(t *testing.T, db *sql.DB) *sqlite.Runtime {
	t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: db, Logger: slog.New(slog.DiscardHandler), PollInterval: suiteTick, HostTTL: suiteHostTTL, StopGrace: 5 * time.Second})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	return rt
}

func serveRuntime(t *testing.T, rt *sqlite.Runtime) {
	t.Helper()
	hostCtx, stopHost := context.WithCancel(context.WithoutCancel(t.Context()))
	hosted := make(chan error, 1)
	go func() { hosted <- rt.Run(hostCtx) }()
	t.Cleanup(func() {
		stopHost()
		select {
		case err := <-hosted:
			if err != nil {
				t.Errorf("durable host: %v", err)
			}
		case <-time.After(suiteStopBound):
			t.Error("the durable host did not stop")
		}
	})
}

type ledgerTurn struct {
	scenario turnScenario
	ledger   *turnLedger
	options  []runtimeadk.ExecutorOption
}

type succeededGate struct {
	ledger *turnLedger
}

func (g succeededGate) Observe(ctx context.Context, observation *effect.Observation) {
	if observation.Outcome == string(effect.StateSucceeded) {
		_ = g.ledger.gate(ctx, observation.Operation+"-succeeded")
	}
}

func newGatedEffects(t *testing.T, db *sql.DB, ledger *turnLedger) *effect.Executor {
	t.Helper()
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler), Observer: succeededGate{ledger: ledger}})
	if err != nil {
		t.Fatalf("effect.NewJournal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("effect.NewExecutor: %v", err)
	}
	return executor
}

func (s *turnScenario) hasEffectfulTool() bool {
	return slices.ContainsFunc(s.tools, func(stub stubTool) bool { return stub.effectful })
}

func newLedgerExecutor(t *testing.T, db *sql.DB, parts *ledgerTurn) *runtimeadk.ADKExecutor {
	t.Helper()
	options := &toolbroker.Options{HostCapabilities: capabilitytest.Host(t), ApprovalDecider: parts.ledger.decider(parts.scenario.approvingLife)}
	if parts.scenario.hasEffectfulTool() {
		options.Effects = newGatedEffects(t, db, parts.ledger)
	}
	broker := toolbrokertest.NewBroker(t, options)
	tools := &ledgerTools{broker: broker, ledger: parts.ledger, gated: parts.scenario.gated}
	for _, stub := range parts.scenario.tools {
		toolbrokertest.Register(t, broker, toolbrokertest.Stub{Name: stub.name, RequiresApproval: stub.approval, Effectful: stub.effectful, Run: parts.ledger.adapter(stub)})
		tools.defs = append(tools.defs, runtimeadk.BuiltinToolDefinition{Name: stub.name, Version: stubToolVersion, Schema: json.RawMessage(`{"type":"object"}`)})
	}
	opts := append([]runtimeadk.ExecutorOption{runtimeadk.WithBuiltinToolExecutor(tools)}, parts.options...)
	model := &scriptedModel{ledger: parts.ledger, steps: parts.scenario.steps}
	executor, err := runtimeadk.NewADKExecutor("aura", model.Name(), auramodel.Set{model.Name(): model}, store.NewSessionService(db), tools, nil, slog.New(slog.DiscardHandler), opts...)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	return executor
}

func openLife(t *testing.T, plan lifePlan) *turnLife {
	t.Helper()
	scenario, ok := turnScenarios[plan.Scenario]
	if !ok {
		t.Fatalf("unknown scenario %q", plan.Scenario)
	}
	db, err := store.OpenDB(t.Context(), plan.Database)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.DiscardHandler)
	ledger := &turnLedger{dir: plan.Ledger, life: plan.Life}
	rt := newSuiteRuntime(t, db)
	sessions, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db})
	if err != nil {
		t.Fatalf("sqlite.NewSessionStore: %v", err)
	}
	var wiring lifeWiring
	if scenario.wiring != nil {
		wiring = scenario.wiring(t, ledger)
	}
	var opts []runtimeadk.ExecutorOption
	if wiring.skillContext != nil {
		opts = append(opts, runtimeadk.WithSkillContext(wiring.skillContext))
	}
	if wiring.knowledge != nil {
		opts = append(opts, runtimeadk.WithKnowledgeContext(wiring.knowledge))
	}
	executor := newLedgerExecutor(t, db, &ledgerTurn{scenario: scenario, ledger: ledger, options: opts})
	events := store.NewEventStore(db)
	engine, err := runtimeengine.NewEngine(&runtimeengine.Config{
		MaxActiveTurns:  4,
		MaxPendingTurns: 16,
		TurnTimeout:     5 * time.Minute,
		DefaultAgentID:  "main",
		Skills:          wiring.renderer,
		Durable:         &runtimeengine.DurableConfig{Sessions: sessions, Runtime: rt},
		LiveEvents:      turn.NewEventBus(logger),
	}, events, store.NewDedupeStore(db), executor, logger)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	serveRuntime(t, rt)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), suiteStopBound)
		defer cancel()
		if err := engine.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shut the engine down: %v", err)
		}
	})
	return &turnLife{scenario: scenario, db: db, runtime: rt, sessions: sessions, engine: engine, ledger: ledger}
}

func (l *turnLife) submit(t *testing.T, req *turn.TurnRequest) <-chan error {
	t.Helper()
	request := *req
	ctx := context.WithoutCancel(t.Context())
	out := make(chan error, 1)
	go func() {
		var streamErr error
		for _, err := range l.engine.Run(ctx, &request) {
			if err != nil {
				streamErr = err
				break
			}
		}
		out <- streamErr
	}()
	return out
}

func (l *turnLife) requireActivity(t *testing.T, activeTurnID string, queued int) {
	t.Helper()
	if active, n := l.engine.SessionActivity(suiteSession); active != activeTurnID || n != queued {
		t.Fatalf("SessionActivity = (%q, %d), want (%q, %d)", active, n, activeTurnID, queued)
	}
}

func (l *turnLife) waitActivity(t *testing.T, activeTurnID string, queued int) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("SessionActivity is (%q, %d)", activeTurnID, queued), func() bool {
		active, n := l.engine.SessionActivity(suiteSession)
		return active == activeTurnID && n == queued
	})
}

func suiteRequest(turnID, text string) turn.TurnRequest {
	return turn.TurnRequest{
		TurnID:         turnID,
		SessionID:      suiteSession,
		PrincipalID:    suiteOwner,
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: text}},
	}
}

func requestText(req *turn.TurnRequest) string {
	var text strings.Builder
	for _, part := range req.Parts {
		text.WriteString(part.Text)
	}
	return text.String()
}

func seedSuite(t *testing.T, scenario string) (lifePlan, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	plan := lifePlan{Database: filepath.Join(dir, "aura.db"), Scenario: scenario, Ledger: filepath.Join(dir, "ledger"), Life: 1}
	if err := os.Mkdir(plan.Ledger, 0o700); err != nil {
		t.Fatalf("create the ledger directory: %v", err)
	}
	storetest.Seed(t, plan.Database)
	db, err := store.OpenDB(t.Context(), plan.Database)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: suiteSession, OwnerID: suiteOwner}); err != nil {
		t.Fatalf("create %s: %v", suiteSession, err)
	}
	return plan, db
}

func waitUntil(t *testing.T, what string, reached func() bool) {
	t.Helper()
	deadline := time.NewTimer(suiteDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(suiteTick)
	defer tick.Stop()
	for !reached() {
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
	case <-time.After(suiteDeadline):
		t.Fatal("the turn stream never ended")
	}
}

type firstLife struct {
	cmd    *exec.Cmd
	output *bytes.Buffer
	exited chan error
	ended  bool
}

type lifeEntry struct {
	test string
	env  string
}

var turnEntry = lifeEntry{test: "TestDurableTurnFirstLife", env: firstLifeEnv}

type lifeLaunch struct {
	entry lifeEntry
	plan  any
}

func startFirstLife(t *testing.T, launch lifeLaunch) *firstLife {
	t.Helper()
	encoded, err := json.Marshal(launch.plan)
	if err != nil {
		t.Fatalf("encode the first life plan: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+launch.entry.test+"$", "-test.timeout=3m", "-test.v")
	cmd.Env = append(os.Environ(), launch.entry.env+"="+string(encoded), "GORACE=halt_on_error=1")
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the first life: %v", err)
	}
	child := &firstLife{cmd: cmd, output: output, exited: make(chan error, 1)}
	go func() { child.exited <- cmd.Wait() }()
	t.Cleanup(func() {
		child.stop(t)
		if t.Failed() {
			t.Logf("first life output:\n%s", output.String())
		}
	})
	return child
}

func (c *firstLife) receive(t *testing.T) {
	t.Helper()
	select {
	case <-c.exited:
		c.ended = true
	case <-time.After(suiteDeadline):
		t.Fatal("the first life did not end")
	}
}

func (c *firstLife) stop(t *testing.T) {
	t.Helper()
	if c.ended {
		return
	}
	if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill the first life: %v", err)
	}
	c.receive(t)
}

func (c *firstLife) waitKillPoint(t *testing.T, what string, reached func() bool) {
	t.Helper()
	deadline := time.NewTimer(suiteDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(suiteTick)
	defer tick.Stop()
	for !reached() {
		select {
		case err := <-c.exited:
			c.ended = true
			t.Fatalf("the first life exited before %s: %v", what, err)
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func (c *firstLife) kill(t *testing.T) {
	t.Helper()
	if c.ended {
		t.Fatal("the first life ended before the kill")
	}
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the first life: %v", err)
	}
	c.receive(t)
	status, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the first life ended with %v, want SIGKILL", c.cmd.ProcessState)
	}
}

type killedSuite struct {
	plan     lifePlan
	scenario turnScenario
	db       *sql.DB
	ledger   *turnLedger
	child    *firstLife
}

func startKilledSuite(t *testing.T, scenario string) *killedSuite {
	t.Helper()
	plan, db := seedSuite(t, scenario)
	return &killedSuite{
		plan:     plan,
		scenario: turnScenarios[scenario],
		db:       db,
		ledger:   &turnLedger{dir: plan.Ledger},
		child:    startFirstLife(t, lifeLaunch{entry: turnEntry, plan: plan}),
	}
}

func (s *killedSuite) resume(t *testing.T) *turnLife {
	t.Helper()
	s.child.kill(t)
	plan := s.plan
	plan.Life = 2
	life := openLife(t, plan)
	if err := life.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns: %v", err)
	}
	return life
}

func (s *killedSuite) release(t *testing.T, tool string) {
	t.Helper()
	s.ledger.mark(t, tool+".release")
}

func (s *killedSuite) requireResumed(t *testing.T, life *turnLife) {
	t.Helper()
	texts := make([]string, 0, len(s.scenario.requests))
	for i := range s.scenario.requests {
		req := &s.scenario.requests[i]
		texts = append(texts, requestText(req))
		events := waitTerminal(t, s.db, req.TurnID)
		requireCompletedOnce(t, events)
		if n := storedUserText(t, events, requestText(req)); n != 1 {
			t.Errorf("stored ADK events of %s hold the user's text %d times, want 1", req.TurnID, n)
		}
	}
	s.requireRequestsAfterKill(t, texts)
	life.waitActivity(t, "", 0)
	for i := range s.scenario.requests {
		turnID := s.scenario.requests[i].TurnID
		if state, _ := runRow(t, s.db, turnID); state != string(durable.RunSucceeded) {
			t.Errorf("run %s state = %q, want %q", turnID, state, durable.RunSucceeded)
		}
	}
	first := s.scenario.requests[0].TurnID
	if _, claim := runRow(t, s.db, first); claim != 2 {
		t.Errorf("run %s claim = %d, want 2 (one claim by each life; more means a host lost its lease under load and an action may have run twice)", first, claim)
	}
}

func (s *killedSuite) requireRequestsAfterKill(t *testing.T, texts []string) {
	t.Helper()
	var after []ledgerRecord
	for _, rec := range s.ledger.records(t, modelCalls) {
		if rec.Life == 2 {
			after = append(after, rec)
		}
	}
	if len(after) == 0 {
		t.Fatal("no model request was made after the kill")
	}
	for i, rec := range after {
		for j, text := range texts {
			n := strings.Count(rec.Text, text)
			if n > 1 || ((j == 0 || i == len(after)-1) && n != 1) {
				t.Errorf("model request %d after the kill holds %q %d times:\n%s", i+1, text, n, rec.Text)
			}
		}
	}
}

func turnEvents(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	var sessionID string
	err := db.QueryRowContext(t.Context(), `SELECT session_id FROM runtime_event WHERE turn_id = ? AND kind = ?`, turnID, turn.EventKindTurnAccepted).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the session of %s: %v", turnID, err)
	}
	events, err := store.NewDedupeStore(db).ListTurnEvents(t.Context(), sessionID, turnID)
	if err != nil {
		t.Fatalf("list the events of %s: %v", turnID, err)
	}
	return events
}

func isTerminalKind(kind string) bool {
	switch kind {
	case turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled:
		return true
	}
	return false
}

func waitTerminal(t *testing.T, db *sql.DB, turnID string) []store.RuntimeEvent {
	t.Helper()
	var events []store.RuntimeEvent
	waitUntil(t, turnID+" has a terminal event", func() bool {
		events = turnEvents(t, db, turnID)
		for i := range events {
			if isTerminalKind(events[i].Kind) {
				return true
			}
		}
		return false
	})
	return events
}

func countKind(events []store.RuntimeEvent, kind string) int {
	n := 0
	for i := range events {
		if events[i].Kind == kind {
			n++
		}
	}
	return n
}

func terminalEvents(events []store.RuntimeEvent) []store.RuntimeEvent {
	var terminals []store.RuntimeEvent
	for i := range events {
		if isTerminalKind(events[i].Kind) {
			terminals = append(terminals, events[i])
		}
	}
	return terminals
}

func requireCompletedOnce(t *testing.T, events []store.RuntimeEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("the turn has no events")
	}
	if last := events[len(events)-1]; last.Kind != turn.EventKindTurnCompleted {
		t.Errorf("last event of %s = %q, want %q", last.TurnID, last.Kind, turn.EventKindTurnCompleted)
	}
	if terminals := terminalEvents(events); len(terminals) != 1 {
		t.Errorf("terminal events = %d, want 1", len(terminals))
	}
	if n := countKind(events, turn.EventKindTurnAccepted); n != 1 {
		t.Errorf("turn.accepted events = %d, want 1", n)
	}
}

func storedUserText(t *testing.T, events []store.RuntimeEvent, text string) int {
	t.Helper()
	n := 0
	for i := range events {
		if events[i].Kind != store.EventKindADK {
			continue
		}
		ev, err := store.RuntimeEventToADK(&events[i])
		if err != nil {
			t.Fatalf("decode the stored ADK event %s: %v", events[i].ID, err)
		}
		if ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part != nil {
				n += strings.Count(part.Text, text)
			}
		}
	}
	return n
}

func runRow(t *testing.T, db *sql.DB, key string) (state string, claim int) {
	t.Helper()
	if err := db.QueryRowContext(t.Context(), `SELECT state, claim FROM durable_run WHERE key = ?`, key).Scan(&state, &claim); err != nil {
		t.Fatalf("read the run %s: %v", key, err)
	}
	return state, claim
}
