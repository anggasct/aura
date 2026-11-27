package runtimeengine

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

	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

const handoffToolName = "handoff_to_owner"

func repliesToHandoff(toolName string) bool {
	return toolName == handoffToolName
}

func defaultMessage(key string) string {
	return agent.PersonaMessageDefaults()[key]
}

func (a *capturingAdapter) texts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, req := range a.requests {
		var parts []string
		for _, part := range req.Parts {
			parts = append(parts, part.Text)
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	return out
}

func distinct(values []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}

type toolResult struct {
	name     string
	response map[string]any
}

func functionResponseStep(results ...toolResult) turntest.FakeStep {
	parts := make([]any, 0, len(results))
	for _, result := range results {
		parts = append(parts, map[string]any{"functionResponse": map[string]any{"name": result.name, "response": result.response}})
	}
	raw, err := json.Marshal(map[string]any{"content": map[string]any{"role": "user", "parts": parts}})
	if err != nil {
		panic("test payload is not serializable")
	}
	return turntest.FakeStep{Kind: turnstore.EventKindADK, Payload: raw}
}

func actionRecordsOf(t *testing.T, db *sql.DB, turnID, action string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT CAST(result AS TEXT) FROM durable_action WHERE run_key = ? AND action_key = ?`, turnID, action)
	if err != nil {
		t.Fatalf("read the %s records of %s: %v", action, turnID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan a %s record of %s: %v", action, turnID, err)
		}
		out = append(out, result)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s records of %s: %v", action, turnID, err)
	}
	return out
}

func finalMessageOf(t *testing.T, db *sql.DB, turnID string) finalMessage {
	t.Helper()
	records := actionRecordsOf(t, db, turnID, finalMessageAction)
	if len(records) != 1 {
		t.Fatalf("final-message records of %s = %v, want one", turnID, records)
	}
	var final finalMessage
	if err := json.Unmarshal([]byte(records[0]), &final); err != nil {
		t.Fatalf("decode the final-message record %s: %v", records[0], err)
	}
	return final
}

func runFinalTurn(t *testing.T, deps Deps, adapter runtimechannelhost.ChannelPort, executor TurnExecutor) *durableHostRig {
	t.Helper()
	if deps.Config == (Config{}) {
		deps.Config = testLimits(4, 16)
	}
	deps.Channels = []runtimechannelhost.ChannelPort{adapter}
	rig := newDurableHostRig(t, deps, executor)
	mustCreateSession(t, rig.db, "channel:333")
	rig.startHost()
	_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
	awaitTerminals(t, rig.db, "turn-1")
	return rig
}

type finalTurnCase struct {
	name         string
	timeout      time.Duration
	executor     TurnExecutor
	wantKind     finalKind
	wantMessage  string
	wantTerminal terminalRecord
}

func (tc *finalTurnCase) deps() Deps {
	deps := Deps{RepliesToRequester: repliesToHandoff}
	if tc.timeout > 0 {
		deps.Config = testLimits(4, 16).withTurnTimeout(tc.timeout)
	}
	return deps
}

func requireFinalTurn(t *testing.T, rig *durableHostRig, capture *capturingAdapter, tc *finalTurnCase) {
	t.Helper()
	got := terminalRecordOf(t, rig.db, "turn-1")
	if got.kind != tc.wantTerminal.kind || got.code != tc.wantTerminal.code {
		t.Fatalf("terminal of turn-1 = %+v, want %+v", got, tc.wantTerminal)
	}
	final := finalMessageOf(t, rig.db, "turn-1")
	if final.Kind != tc.wantKind || final.Code != tc.wantTerminal.code {
		t.Fatalf("final-message record = %+v, want kind %s code %s", final, tc.wantKind, tc.wantTerminal.code)
	}
	if tc.wantMessage == "" {
		if got := capture.count(); got != 0 {
			t.Fatalf("Deliver calls = %d, want 0", got)
		}
		if len(final.Parts) != 0 {
			t.Fatalf("final-message parts = %v, want none", final.Parts)
		}
		return
	}
	if got := capture.texts(); !slices.Equal(got, []string{tc.wantMessage}) {
		t.Fatalf("delivered texts = %q, want only %q", got, tc.wantMessage)
	}
	if !slices.Equal(final.Parts, []string{tc.wantMessage}) {
		t.Fatalf("final-message parts = %q, want only %q", final.Parts, tc.wantMessage)
	}
}

func TestFailedTurnDeliversOneFallback(t *testing.T) {
	cases := []finalTurnCase{
		{
			name:         "the deadline",
			timeout:      time.Nanosecond,
			executor:     turntest.NewFakeExecutor([]turntest.FakeStep{textStep("too late")}),
			wantKind:     finalTurnTimeout,
			wantMessage:  defaultMessage(agent.MessageTurnTimeout),
			wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeTurnDeadlineExceeded},
		},
		{
			name:         "an executor error without a code",
			executor:     errorExecutor{err: errors.New("provider exploded")},
			wantKind:     finalTurnFailed,
			wantMessage:  defaultMessage(agent.MessageTurnFailed),
			wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeRuntimeInternal},
		},
		{
			name:         "an exhausted budget",
			executor:     errorExecutor{err: fault.New(turn.ErrorCodeBudgetExhausted, "budget")},
			wantKind:     finalTurnFailed,
			wantMessage:  defaultMessage(agent.MessageTurnFailed),
			wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeBudgetExhausted},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &capturingAdapter{}
			rig := runFinalTurn(t, tc.deps(), capture, tc.executor)
			requireFinalTurn(t, rig, capture, &tc)
		})
	}
}

func TestEmptyTurnDeliversTurnEmpty(t *testing.T) {
	handoffDone := toolResult{name: handoffToolName, response: map[string]any{"result": "handed"}}
	handoffFailed := toolResult{name: handoffToolName, response: map[string]any{"error": "no owner"}}
	otherFailed := toolResult{name: "read_file", response: map[string]any{"error": "missing"}}
	completed := terminalRecord{kind: turn.EventKindTurnCompleted}
	cases := []finalTurnCase{
		{
			name:         "a completed turn without text",
			executor:     turntest.NewFakeExecutor([]turntest.FakeStep{jsonStep(turn.EventKindMessageCompleted)}),
			wantKind:     finalTurnEmpty,
			wantMessage:  defaultMessage(agent.MessageTurnEmpty),
			wantTerminal: completed,
		},
		{
			name:         "a successful handoff",
			executor:     turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(handoffDone)}),
			wantKind:     finalNone,
			wantTerminal: completed,
		},
		{
			name:         "a successful handoff in an event next to a failed call",
			executor:     turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(otherFailed, handoffDone)}),
			wantKind:     finalNone,
			wantTerminal: completed,
		},
		{
			name:         "a failed handoff",
			executor:     turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(handoffFailed)}),
			wantKind:     finalTurnEmpty,
			wantMessage:  defaultMessage(agent.MessageTurnEmpty),
			wantTerminal: completed,
		},
		{
			name:         "a failed handoff next to a successful call of another tool",
			executor:     turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(handoffFailed, toolResult{name: "read_file", response: map[string]any{"result": "ok"}})}),
			wantKind:     finalTurnEmpty,
			wantMessage:  defaultMessage(agent.MessageTurnEmpty),
			wantTerminal: completed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &capturingAdapter{}
			rig := runFinalTurn(t, tc.deps(), capture, tc.executor)
			requireFinalTurn(t, rig, capture, &tc)
		})
	}
	t.Run("a turn without the predicate", func(t *testing.T) {
		capture := &capturingAdapter{}
		rig := runFinalTurn(t, Deps{}, capture, turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(handoffDone)}))
		requireFinalTurn(t, rig, capture, &finalTurnCase{wantKind: finalTurnEmpty, wantMessage: defaultMessage(agent.MessageTurnEmpty), wantTerminal: completed})
	})
}

type crashInFinalMessage struct {
	once   sync.Once
	hand   func()
	handed chan struct{}
}

func (p *crashInFinalMessage) Fingerprint(string) (turn.PersonaFingerprint, bool) {
	return turn.PersonaFingerprint{}, false
}

func (p *crashInFinalMessage) Knowledge(string, principal.Class) []string { return nil }

func (p *crashInFinalMessage) Message(string, string) string {
	p.once.Do(func() {
		p.hand()
		close(p.handed)
	})
	return ""
}

func (p *crashInFinalMessage) await(t *testing.T) {
	t.Helper()
	select {
	case <-p.handed:
	case <-time.After(hostRigTimeout):
		t.Fatal("the first life never reached the final message")
	}
}

type crashSite string

const (
	crashAfterTheSend      crashSite = "after the send"
	crashBeforeTheDecision crashSite = "before the decision was recorded"
)

type replayCase struct {
	name          string
	crash         crashSite
	first         TurnExecutor
	second        TurnExecutor
	secondTimeout time.Duration
	wantText      string
	wantKind      finalKind
	wantTerminal  terminalRecord
}

func replayAcrossACrash(t *testing.T, tc *replayCase) (*durableHostRig, *capturingAdapter) {
	t.Helper()
	db := openTestDatabase(t)
	cfg := Deps{Config: testLimits(4, 16)}
	var capture *capturingAdapter
	var awaitCrash func(*testing.T)
	switch tc.crash {
	case crashAfterTheSend:
		fenced := &fencedDeliveryAdapter{db: db, handed: make(chan struct{})}
		capture, awaitCrash = &fenced.capturingAdapter, fenced.await
		cfg.Channels = []runtimechannelhost.ChannelPort{fenced}
	case crashBeforeTheDecision:
		capture = &capturingAdapter{}
		personas := &crashInFinalMessage{handed: make(chan struct{})}
		personas.hand = func() {
			if err := handRunToGhostHost(t.Context(), db, "turn-1"); err != nil {
				t.Errorf("hand the run to another host: %v", err)
			}
		}
		awaitCrash = personas.await
		cfg.Personas = personas
		cfg.Channels = []runtimechannelhost.ChannelPort{capture}
	}
	rig := &durableHostRig{t: t, cfg: cfg, db: db, sessions: newHookedSessionStore(t, db)}
	rig.build(tc.first)
	mustCreateSession(t, db, "channel:333")
	rig.startHost()
	_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
	awaitCrash(t)
	rig.stopHost()
	if terminals := terminalsOf(t, db, "turn-1"); len(terminals) != 0 {
		t.Fatalf("terminal events after the first life = %v, want none", terminals)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM durable_host WHERE owner = ?`, ghostHostOwner); err != nil {
		t.Fatalf("drop the other host: %v", err)
	}
	if tc.secondTimeout > 0 {
		rig.cfg.Config = rig.cfg.Config.withTurnTimeout(tc.secondTimeout)
	}
	rig.build(tc.second)
	rig.startHost()
	awaitTerminals(t, db, "turn-1")
	return rig, capture
}

func requireReplayedTurn(t *testing.T, rig *durableHostRig, capture *capturingAdapter, tc *replayCase) {
	t.Helper()
	if got := distinct(capture.texts()); !slices.Equal(got, []string{tc.wantText}) {
		t.Fatalf("texts delivered across both lives = %q, want only %q", got, tc.wantText)
	}
	got := terminalRecordOf(t, rig.db, "turn-1")
	if got.kind != tc.wantTerminal.kind || got.code != tc.wantTerminal.code {
		t.Fatalf("terminal of turn-1 = %+v, want %+v", got, tc.wantTerminal)
	}
	if final := finalMessageOf(t, rig.db, "turn-1"); final.Kind != tc.wantKind {
		t.Fatalf("final-message kind = %q, want %q", final.Kind, tc.wantKind)
	}
}

func TestFinalMessageIsDecidedOnce(t *testing.T) {
	cases := []replayCase{
		{
			name:         "an answer sent before the crash, the executor now fails",
			crash:        crashAfterTheSend,
			first:        turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
			second:       errorExecutor{err: errors.New("provider exploded")},
			wantText:     "the answer",
			wantKind:     finalAnswer,
			wantTerminal: terminalRecord{kind: turn.EventKindTurnCompleted},
		},
		{
			name:          "an answer sent before the crash, the deadline has passed",
			crash:         crashAfterTheSend,
			first:         turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
			second:        turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
			secondTimeout: time.Nanosecond,
			wantText:      "the answer",
			wantKind:      finalAnswer,
			wantTerminal:  terminalRecord{kind: turn.EventKindTurnCompleted},
		},
		{
			name:         "a fallback sent before the crash, the executor now succeeds",
			crash:        crashAfterTheSend,
			first:        errorExecutor{err: errors.New("provider exploded")},
			second:       turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
			wantText:     defaultMessage(agent.MessageTurnFailed),
			wantKind:     finalTurnFailed,
			wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeRuntimeInternal},
		},
		{
			name:         "nothing recorded before the crash, the executor now succeeds",
			crash:        crashBeforeTheDecision,
			first:        errorExecutor{err: errors.New("provider exploded")},
			second:       turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
			wantText:     "the answer",
			wantKind:     finalAnswer,
			wantTerminal: terminalRecord{kind: turn.EventKindTurnCompleted},
		},
		{
			name:          "nothing recorded before the crash, the deadline has passed",
			crash:         crashBeforeTheDecision,
			first:         errorExecutor{err: errors.New("provider exploded")},
			second:        turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
			secondTimeout: time.Nanosecond,
			wantText:      defaultMessage(agent.MessageTurnTimeout),
			wantKind:      finalTurnTimeout,
			wantTerminal:  terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeTurnDeadlineExceeded},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, capture := replayAcrossACrash(t, &tc)
			requireReplayedTurn(t, rig, capture, &tc)
		})
	}
}

func TestAnswerSentIsNeverFollowedByAFailedTerminal(t *testing.T) {
	tc := replayCase{
		crash:         crashAfterTheSend,
		first:         turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}),
		second:        errorExecutor{err: errors.New("provider exploded")},
		secondTimeout: time.Nanosecond,
		wantText:      "the answer",
		wantKind:      finalAnswer,
		wantTerminal:  terminalRecord{kind: turn.EventKindTurnCompleted},
	}
	rig, capture := replayAcrossACrash(t, &tc)
	requireReplayedTurn(t, rig, capture, &tc)
}

func TestCancelledTurnSendsNoFinalMessage(t *testing.T) {
	t.Run("a user cancel", func(t *testing.T) {
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		capture := &capturingAdapter{}
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
		rig := newDurableHostRig(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
		eventually(t, "the turn reaches the executor", func() bool { return executor.StartCount() == 1 })
		if _, cancelled, err := rig.engine.CancelActive(t.Context(), "channel:333"); err != nil || !cancelled {
			t.Fatalf("CancelActive = %v, %v; want the turn cancelled", cancelled, err)
		}
		awaitTerminal(t, outcome, turn.EventKindTurnCancelled)
		if got := capture.count(); got != 0 {
			t.Fatalf("Deliver calls = %d, want 0", got)
		}
		if got := actionRecordsOf(t, rig.db, "turn-1", finalMessageAction); len(got) != 0 {
			t.Fatalf("final-message records = %v, want none", got)
		}
	})
	t.Run("a preemption", func(t *testing.T) {
		capture := &capturingAdapter{}
		executor := newTwoInvocationExecutor()
		rig := newDurableHostRig(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
		executor.waitBlocked(t)
		rig.stopHost()
		if got := capture.count(); got != 0 {
			t.Fatalf("Deliver calls after the preemption = %d, want 0", got)
		}
		if got := actionRecordsOf(t, rig.db, "turn-1", finalMessageAction); len(got) != 0 {
			t.Fatalf("final-message records after the preemption = %v, want none", got)
		}
		rig.build(executor)
		rig.startHost()
		awaitTerminals(t, rig.db, "turn-1")
		if got := capture.texts(); !slices.Equal(got, []string{"ok"}) {
			t.Fatalf("delivered texts after the resume = %q, want the resumed answer only", got)
		}
	})
}

type lateCancelError struct{}

func (lateCancelError) Error() string        { return "context canceled after the executor returned" }
func (lateCancelError) Is(target error) bool { return target == context.Canceled }

type lateCancelContext struct {
	done chan struct{}
	once sync.Once
	racy atomic.Bool
}

func newLateCancelContext() *lateCancelContext {
	return &lateCancelContext{done: make(chan struct{})}
}

func (c *lateCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *lateCancelContext) Done() <-chan struct{} { return c.done }

func (c *lateCancelContext) Value(any) any { return nil }

func (c *lateCancelContext) Err() error {
	if c.racy.Load() {
		return lateCancelError{}
	}
	return nil
}

func (c *lateCancelContext) cancel() {
	c.once.Do(func() {
		c.racy.Store(true)
		close(c.done)
	})
}

type executorCancelsLate struct {
	inner TurnExecutor
	late  *lateCancelContext
}

func (x *executorCancelsLate) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error] {
	return func(yield func(turn.Event, error) bool) {
		for ev, err := range x.inner.Execute(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
		x.late.cancel()
		select {
		case <-ctx.Done():
		case <-time.After(hostRigTimeout):
		}
	}
}

type lateCancelInvocation struct {
	payload []byte
	mu      sync.Mutex
	actions map[string][]byte
}

func (i *lateCancelInvocation) Run() durable.RunRef { return durable.RunRef{Key: "turn-1"} }

func (i *lateCancelInvocation) Payload() []byte { return i.payload }

func (i *lateCancelInvocation) Signal(context.Context, string) ([]byte, bool) { return nil, false }

func (i *lateCancelInvocation) Sleep(time.Duration) error { return nil }

func (i *lateCancelInvocation) Timer(time.Duration) <-chan time.Time { return make(chan time.Time) }

func (i *lateCancelInvocation) Wait(context.Context, string, time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}

func (i *lateCancelInvocation) RunAction(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	i.mu.Lock()
	if raw, ok := i.actions[key]; ok {
		i.mu.Unlock()
		return raw, nil
	}
	i.mu.Unlock()
	raw, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.actions[key] = raw
	return raw, nil
}

func TestCancelledTurnEndAfterTheExecutorSendsNoFinalMessage(t *testing.T) {
	capture := &capturingAdapter{}
	hook, counter := &recordingHook{}, &recordingCounter{}
	late := newLateCancelContext()
	executor := &executorCancelsLate{inner: turntest.NewFakeExecutor([]turntest.FakeStep{textStep("the answer")}), late: late}
	rig := newDurableHostRig(t, Deps{
		Config:       testLimits(4, 16),
		CountTurn:    counter.fn,
		PostTurnHook: hook.fn,
		Channels:     []runtimechannelhost.ChannelPort{capture},
	}, executor)
	mustCreateSession(t, rig.db, "channel:333")
	req := channelTurnRequest("channel:333", "turn-1")
	admitThroughStore(t, rig.sessions, req)
	desc := turnDescriptorFromRequest(req)
	payload, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	inv := &lateCancelInvocation{payload: payload, actions: map[string][]byte{}}
	if err := rig.engine.DriveTurn(late, inv, &desc); !errors.Is(err, context.Canceled) {
		t.Fatalf("DriveTurn = %v, want a cancelled turn end", err)
	}
	if got := capture.count(); got != 0 {
		t.Fatalf("Deliver calls = %d, want 0", got)
	}
	if got := actionRecordsOf(t, rig.db, "turn-1", finalMessageAction); len(got) != 0 {
		t.Fatalf("final-message records = %v, want none", got)
	}
	if got := terminalsOf(t, rig.db, "turn-1"); len(got) != 0 {
		t.Fatalf("terminal events = %v, want none", got)
	}
	requireCalls(t, hook, counter, 1, 1)
}

func TestFallbackDeliveryFailureKeepsTheTurnCode(t *testing.T) {
	logs := &lockedBuffer{}
	capture := &capturingAdapter{deliverErr: errDeliveryBoom}
	deps := Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}
	rig := newDurableHostRig(t, deps, errorExecutor{err: fault.New(turn.ErrorCodeBudgetExhausted, "budget")})
	rig.engine.logger = slog.New(slog.NewTextHandler(logs, nil))
	mustCreateSession(t, rig.db, "channel:333")
	rig.startHost()
	_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
	awaitTerminals(t, rig.db, "turn-1")
	if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnFailed || got.code != turn.ErrorCodeBudgetExhausted {
		t.Fatalf("terminal of turn-1 = %+v, want turn.failed with %s", got, turn.ErrorCodeBudgetExhausted)
	}
	records := deliveryRecordsOf(t, rig.db, "turn-1")
	if len(records) != 1 || !strings.HasPrefix(records[0], `{"outcome":"failed","code":"delivery_failed"`) {
		t.Fatalf("delivery records = %v, want one failed record", records)
	}
	if got := strings.Count(logs.String(), "turn fallback delivery failed"); got != 1 || !strings.Contains(logs.String(), "turn_id=turn-1") || !strings.Contains(logs.String(), "code=delivery_failed") {
		t.Fatalf("the fallback failure was logged %d times, want once with the turn id and code: %q", got, logs.String())
	}
}

func TestEmptyFallbackDeliveryFailureFailsTheTurn(t *testing.T) {
	capture := &capturingAdapter{deliverErr: errDeliveryBoom}
	rig := runFinalTurn(t, Deps{}, capture, turntest.NewFakeExecutor([]turntest.FakeStep{jsonStep(turn.EventKindMessageCompleted)}))
	if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnFailed || got.code != turn.ErrorCodeDeliveryFailed {
		t.Fatalf("terminal of turn-1 = %+v, want turn.failed with %s", got, turn.ErrorCodeDeliveryFailed)
	}
	if got := finalMessageOf(t, rig.db, "turn-1"); got.Kind != finalTurnEmpty {
		t.Fatalf("final-message kind = %q, want %q", got.Kind, finalTurnEmpty)
	}
}

type memoryCase struct {
	name         string
	timeout      time.Duration
	executor     TurnExecutor
	wantMessage  string
	wantTerminal terminalRecord
}

func TestInMemoryTurnDeliversTheSameFallback(t *testing.T) {
	handoffDone := toolResult{name: handoffToolName, response: map[string]any{"result": "handed"}}
	handoffFailed := toolResult{name: handoffToolName, response: map[string]any{"error": "no owner"}}
	completed := terminalRecord{kind: turn.EventKindTurnCompleted}
	cases := []memoryCase{
		{name: "the deadline", timeout: time.Nanosecond, executor: turntest.NewFakeExecutor([]turntest.FakeStep{textStep("late")}), wantMessage: defaultMessage(agent.MessageTurnTimeout), wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeTurnDeadlineExceeded}},
		{name: "an executor error without a code", executor: errorExecutor{err: errors.New("provider exploded")}, wantMessage: defaultMessage(agent.MessageTurnFailed), wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeRuntimeInternal}},
		{name: "an exhausted budget", executor: errorExecutor{err: fault.New(turn.ErrorCodeBudgetExhausted, "budget")}, wantMessage: defaultMessage(agent.MessageTurnFailed), wantTerminal: terminalRecord{kind: turn.EventKindTurnFailed, code: turn.ErrorCodeBudgetExhausted}},
		{name: "a completed turn without text", executor: turntest.NewFakeExecutor([]turntest.FakeStep{jsonStep(turn.EventKindMessageCompleted)}), wantMessage: defaultMessage(agent.MessageTurnEmpty), wantTerminal: completed},
		{name: "a successful handoff", executor: turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(handoffDone)}), wantTerminal: completed},
		{name: "a failed handoff", executor: turntest.NewFakeExecutor([]turntest.FakeStep{functionResponseStep(handoffFailed)}), wantMessage: defaultMessage(agent.MessageTurnEmpty), wantTerminal: completed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &capturingAdapter{}
			cfg := testLimits(2, 4)
			if tc.timeout > 0 {
				cfg = cfg.withTurnTimeout(tc.timeout)
			}
			engine, db, _ := newTestRuntime(t, Deps{Config: cfg, RepliesToRequester: repliesToHandoff, Channels: []runtimechannelhost.ChannelPort{capture}}, tc.executor)
			mustCreateSession(t, db, "channel:333")
			events, err := collect(t, engine, channelTurnRequest("channel:333", "turn-1"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			last := events[len(events)-1]
			var payload struct {
				Code turn.ErrorCode `json:"code"`
			}
			if err := json.Unmarshal(last.Payload, &payload); err != nil {
				t.Fatalf("decode the terminal payload: %v", err)
			}
			if last.Kind != tc.wantTerminal.kind || payload.Code != tc.wantTerminal.code {
				t.Fatalf("terminal = %s %q, want %+v", last.Kind, payload.Code, tc.wantTerminal)
			}
			want := []string(nil)
			if tc.wantMessage != "" {
				want = []string{tc.wantMessage}
			}
			if got := capture.texts(); !slices.Equal(got, want) {
				t.Fatalf("delivered texts = %q, want %q", got, want)
			}
		})
	}
}

type fixedMessagePersonas string

func (p fixedMessagePersonas) Fingerprint(string) (turn.PersonaFingerprint, bool) {
	return turn.PersonaFingerprint{}, false
}

func (p fixedMessagePersonas) Knowledge(string, principal.Class) []string { return nil }

func (p fixedMessagePersonas) Message(string, string) string { return string(p) }

func TestFallbackTextIsNeverEmpty(t *testing.T) {
	cases := []struct {
		name     string
		personas turn.Personas
		agentID  string
		want     string
	}{
		{name: "no personas", want: defaultMessage(agent.MessageTurnFailed)},
		{name: "an unknown agent", personas: agent.RegistryPersonas{}, agentID: "ghost", want: defaultMessage(agent.MessageTurnFailed)},
		{name: "a persona that returns nothing", personas: fixedMessagePersonas("  "), want: defaultMessage(agent.MessageTurnFailed)},
		{name: "a persona override", personas: fixedMessagePersonas("Maaf, ada masalah."), want: "Maaf, ada masalah."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &capturingAdapter{}
			deps := Deps{Config: testLimits(2, 4), Personas: tc.personas, Channels: []runtimechannelhost.ChannelPort{capture}}
			engine, db, _ := newTestRuntime(t, deps, errorExecutor{err: errors.New("provider exploded")})
			mustCreateSession(t, db, "channel:333")
			req := channelTurnRequest("channel:333", "turn-1")
			req.AgentID = tc.agentID
			req.Persona = &turn.PersonaFingerprint{AgentID: tc.agentID}
			if _, err := collect(t, engine, req); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := capture.texts(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("delivered texts = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmailTurnGetsTheFallback(t *testing.T) {
	capture := &capturingAdapter{}
	deps := Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{&namedCapturingAdapter{capturingAdapter: capture, name: "email"}}}
	rig := newDurableHostRig(t, deps, errorExecutor{err: errors.New("provider exploded")})
	mustCreateSession(t, rig.db, "channel:333")
	rig.startHost()
	req := channelTurnRequest("channel:333", "turn-1")
	req.Origin = turn.Origin("email")
	_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, req)
	awaitTerminals(t, rig.db, "turn-1")
	if got := capture.texts(); !slices.Equal(got, []string{defaultMessage(agent.MessageTurnFailed)}) {
		t.Fatalf("delivered texts = %q, want the one turn_failed reply", got)
	}
	if got := capture.first().Channel; got != "email" {
		t.Fatalf("delivery channel = %q, want email", got)
	}
}

func TestStartFailureSendsTheFallbackOnce(t *testing.T) {
	failStart := func(t *testing.T, origin turn.Origin, attempts int) *capturingAdapter {
		t.Helper()
		capture := &capturingAdapter{}
		engine, _, db := newDurableLiveTestRuntime(t, Deps{Config: testLimits(2, 4), Channels: []runtimechannelhost.ChannelPort{capture}}, turntest.NewFakeExecutor(nil))
		mustCreateSession(t, db, "channel:333")
		req := channelTurnRequest("channel:333", "turn-1")
		req.Origin = origin
		state := &turnState{req: *req, turnID: req.TurnID, accepted: turn.Event{ID: "accepted", TurnID: req.TurnID, Kind: turn.EventKindTurnAccepted}}
		for range attempts {
			engine.failDurableStart(t.Context(), state, errors.New("start refused"))
		}
		return capture
	}
	t.Run("a channel turn", func(t *testing.T) {
		capture := failStart(t, turn.Origin("discord:test"), 1)
		if got := capture.texts(); !slices.Equal(got, []string{defaultMessage(agent.MessageTurnFailed)}) {
			t.Fatalf("delivered texts = %q, want one turn_failed text", got)
		}
		if got := capture.requests[0].EffectID; got != "turn-1" {
			t.Fatalf("effect id = %q, want the turn id", got)
		}
	})
	t.Run("a turn that already ended", func(t *testing.T) {
		if got := failStart(t, turn.Origin("discord:test"), 2).count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1: a second failure of an ended turn sends nothing", got)
		}
	})
	t.Run("a turn without a channel destination", func(t *testing.T) {
		if got := failStart(t, turn.OriginHTTP, 1).count(); got != 0 {
			t.Fatalf("Deliver calls = %d, want 0", got)
		}
	})
}
