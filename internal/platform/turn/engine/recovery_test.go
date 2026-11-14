package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
	"github.com/anggasct/aura/internal/store"
)

type holdingExecutor struct {
	mu      sync.Mutex
	starts  []string
	inside  int
	holds   map[string]chan struct{}
	onEnter func(turnID string, othersInside int)
}

func newHoldingExecutor() *holdingExecutor {
	return &holdingExecutor{holds: map[string]chan struct{}{}}
}

func (x *holdingExecutor) hold(turnID string) chan struct{} {
	x.mu.Lock()
	defer x.mu.Unlock()
	gate := make(chan struct{})
	x.holds[turnID] = gate
	return gate
}

func (x *holdingExecutor) setOnEnter(fn func(turnID string, othersInside int)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.onEnter = fn
}

func (x *holdingExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		x.mu.Lock()
		x.starts = append(x.starts, req.TurnID)
		others := x.inside
		x.inside++
		gate := x.holds[req.TurnID]
		onEnter := x.onEnter
		x.mu.Unlock()
		defer func() {
			x.mu.Lock()
			x.inside--
			x.mu.Unlock()
		}()
		if onEnter != nil {
			onEnter(req.TurnID, others)
		}
		if !yield(store.RuntimeEvent{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`)}, nil) {
			return
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				yield(store.RuntimeEvent{}, ctx.Err())
				return
			}
		}
		yield(store.RuntimeEvent{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)}, nil)
	}
}

func (x *holdingExecutor) startCount(turnID string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, started := range x.starts {
		if started == turnID {
			n++
		}
	}
	return n
}

func (x *holdingExecutor) startOrder() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.starts)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(hostRigTimeout)
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

func (x *holdingExecutor) waitEntered(t *testing.T, turnID string) {
	t.Helper()
	eventually(t, turnID+" enters the executor", func() bool { return x.startCount(turnID) > 0 })
}

type turnOutcome struct {
	events []store.RuntimeEvent
	err    error
}

func callTurn(ctx context.Context, engine *Engine, req *turn.TurnRequest) <-chan turnOutcome {
	out := make(chan turnOutcome, 1)
	go func() {
		var outcome turnOutcome
		for ev, err := range engine.Run(ctx, req) {
			if err != nil {
				outcome.err = err
				break
			}
			outcome.events = append(outcome.events, ev)
		}
		out <- outcome
	}()
	return out
}

func awaitTerminal(t *testing.T, outcomes <-chan turnOutcome, want string) {
	t.Helper()
	select {
	case outcome := <-outcomes:
		if outcome.err != nil {
			t.Fatalf("turn stream: %v", outcome.err)
		}
		if got := lastKind(outcome.events); got != want {
			t.Fatalf("turn stream ended with %q, want %q", got, want)
		}
	case <-time.After(hostRigTimeout):
		t.Fatalf("the caller never received %s", want)
	}
}

func admitThroughStore(t *testing.T, sessions SessionStore, req *turn.TurnRequest) runtimesessions.AdmitResult {
	t.Helper()
	result, err := sessions.Admit(t.Context(), &runtimesessions.AdmitRequest{
		Turn:            turnDescriptorFromRequest(req),
		AcceptedEventID: "event-" + req.TurnID,
		MaxPending:      16,
	})
	if err != nil {
		t.Fatalf("admit %s through the store: %v", req.TurnID, err)
	}
	return result
}

func startTurnRun(t *testing.T, rt durable.Runtime, req *turn.TurnRequest) {
	t.Helper()
	payload, err := json.Marshal(turnDescriptorFromRequest(req))
	if err != nil {
		t.Fatalf("encode descriptor: %v", err)
	}
	if _, err := rt.Start(t.Context(), durable.StartRequest{Handler: "turn", Key: req.TurnID, Payload: payload}); err != nil {
		t.Fatalf("start the run of %s: %v", req.TurnID, err)
	}
}

func runState(t *testing.T, rt durable.Runtime, turnID string) durable.RunState {
	t.Helper()
	status, err := rt.Status(t.Context(), durable.RunRef{Key: turnID})
	if err != nil {
		t.Fatalf("status of the run of %s: %v", turnID, err)
	}
	return status.State
}

func waitRunEnded(t *testing.T, rt durable.Runtime, turnID string) durable.RunState {
	t.Helper()
	var state durable.RunState
	eventually(t, "the run of "+turnID+" ends", func() bool {
		state = runState(t, rt, turnID)
		return state != durable.RunRunning && state != durable.RunSuspended
	})
	return state
}

type eventFilter struct {
	turnID string
	kinds  []string
}

func eventKinds(t *testing.T, db *sql.DB, filter eventFilter) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT kind FROM runtime_event WHERE turn_id = ? ORDER BY sequence`, filter.turnID)
	if err != nil {
		t.Fatalf("read events of %s: %v", filter.turnID, err)
	}
	defer func() { _ = rows.Close() }()
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan event of %s: %v", filter.turnID, err)
		}
		if len(filter.kinds) == 0 || slices.Contains(filter.kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events of %s: %v", filter.turnID, err)
	}
	return kinds
}

var terminalKinds = []string{turn.EventKindTurnCompleted, turn.EventKindTurnFailed, turn.EventKindTurnCancelled}

func terminalsOf(t *testing.T, db *sql.DB, turnID string) []string {
	t.Helper()
	return eventKinds(t, db, eventFilter{turnID: turnID, kinds: terminalKinds})
}

func awaitTerminals(t *testing.T, db *sql.DB, turnID string) []string {
	t.Helper()
	eventually(t, turnID+" has a terminal event", func() bool { return len(terminalsOf(t, db, turnID)) > 0 })
	return terminalsOf(t, db, turnID)
}

func requireTerminals(t *testing.T, db *sql.DB, want eventFilter) {
	t.Helper()
	if got := awaitTerminals(t, db, want.turnID); !slices.Equal(got, want.kinds) {
		t.Fatalf("terminal events of %s = %v, want %v", want.turnID, got, want.kinds)
	}
}

func storeActiveTurn(t *testing.T, sessions *hookedSessionStore, sessionID string) (activeTurnID string, queued int) {
	t.Helper()
	status, err := sessions.SessionStore.Status(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("status of %s: %v", sessionID, err)
	}
	return status.ActiveTurnID, status.QueueDepth
}

func engineCounts(engine *Engine) (active, staged, pending int) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.active, len(engine.staged), engine.pending
}

func engineKnows(engine *Engine, turnID string) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	_, ok := engine.turns[turnID]
	return ok
}

func TestRecoverTurnsStartsAGrantedTurnWithoutARun(t *testing.T) {
	executor := newHoldingExecutor()
	gate := executor.hold("turn-1")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()
	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))

	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("first RecoverTurns: %v", err)
	}
	executor.waitEntered(t, "turn-1")
	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("second RecoverTurns: %v", err)
	}
	if active, staged, _ := engineCounts(rig.engine); active != 1 || staged != 0 {
		t.Fatalf("engine counts %d active and %d staged turns, want 1 and 0", active, staged)
	}
	close(gate)
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
	if starts := executor.startCount("turn-1"); starts != 1 {
		t.Fatalf("executor started turn-1 %d times, want once", starts)
	}
}

type restartedTurn struct {
	rig      *durableHostRig
	executor *holdingExecutor
	gate     chan struct{}
}

func resumeHeldTurnAfterRestart(t *testing.T) *restartedTurn {
	t.Helper()
	first := newHoldingExecutor()
	first.hold("turn-1")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, first)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()
	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))
	startTurnRun(t, rig.runtime, sampleRequest("session-a", "turn-1"))
	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-2"))
	first.waitEntered(t, "turn-1")
	eventually(t, "the first life writes its first event", func() bool {
		return len(eventKinds(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindModelStarted}})) == 1
	})
	rig.stopHost()
	if state := runState(t, rig.runtime, "turn-1"); state != durable.RunRunning {
		t.Fatalf("run of turn-1 after the host stopped = %s, want running", state)
	}
	if active, _ := storeActiveTurn(t, rig.sessions, "session-a"); active != "turn-1" {
		t.Fatalf("active turn after the host stopped = %q, want turn-1", active)
	}
	if terminals := terminalsOf(t, rig.db, "turn-1"); len(terminals) != 0 {
		t.Fatalf("terminal events after the host stopped = %v, want none", terminals)
	}

	second := newHoldingExecutor()
	gate := second.hold("turn-1")
	rig.restart(second)
	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns: %v", err)
	}
	second.waitEntered(t, "turn-1")
	if active, _ := rig.engine.SessionActivity("session-a"); active != "turn-1" {
		t.Fatalf("SessionActivity reports %q active, want turn-1", active)
	}
	return &restartedTurn{rig: rig, executor: second, gate: gate}
}

func TestRecoverTurnsResumesATurnAfterAHostRestart(t *testing.T) {
	resumed := resumeHeldTurnAfterRestart(t)
	db := resumed.rig.db
	close(resumed.gate)
	requireTerminals(t, db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
	if started := eventKinds(t, db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindModelStarted}}); len(started) != 1 {
		t.Fatalf("turn-1 has %d %s events, want 1", len(started), turn.EventKindModelStarted)
	}
	requireTerminals(t, db, eventFilter{turnID: "turn-2", kinds: []string{turn.EventKindTurnCompleted}})
	if order := resumed.executor.startOrder(); !slices.Equal(order, []string{"turn-1", "turn-2"}) {
		t.Fatalf("second life start order = %v, want turn-1 then turn-2", order)
	}
}

func TestRecoveredTurnCanBeCancelled(t *testing.T) {
	resumed := resumeHeldTurnAfterRestart(t)
	rig := resumed.rig
	turnID, cancelled, err := rig.engine.CancelActive(t.Context(), "session-a")
	if err != nil || !cancelled || turnID != "turn-1" {
		t.Fatalf("CancelActive = %q %v %v, want turn-1 cancelled", turnID, cancelled, err)
	}
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCancelled}})
	if state := waitRunEnded(t, rig.runtime, "turn-1"); state != durable.RunCancelled {
		t.Fatalf("run of turn-1 = %s, want cancelled", state)
	}
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-2", kinds: []string{turn.EventKindTurnCompleted}})
}

func TestRecoverTurnsAttachesRunningTurnsPastTheGate(t *testing.T) {
	executor := newHoldingExecutor()
	executor.hold("turn-a")
	executor.hold("turn-b")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 16, ShutdownTimeout: 200 * time.Millisecond}, executor)
	for _, session := range []string{"session-a", "session-b"} {
		mustCreateSession(t, rig.db, session)
	}
	turnA := sampleRequest("session-a", "turn-a")
	turnB := sampleRequest("session-b", "turn-b")
	for _, req := range []*turn.TurnRequest{turnA, turnB} {
		admitThroughStore(t, rig.sessions, req)
		startTurnRun(t, rig.runtime, req)
	}
	rig.startHost()
	executor.waitEntered(t, "turn-a")
	executor.waitEntered(t, "turn-b")

	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns: %v", err)
	}
	for _, req := range []*turn.TurnRequest{turnA, turnB} {
		if active, queued := rig.engine.SessionActivity(req.SessionID); active != req.TurnID || queued != 0 {
			t.Fatalf("SessionActivity(%s) = %q, %d; want %s active and nothing queued", req.SessionID, active, queued, req.TurnID)
		}
	}
	if active, staged, _ := engineCounts(rig.engine); active != 2 || staged != 0 {
		t.Fatalf("active, staged turns = %d, %d; want both attached turns active past the gate and none staged", active, staged)
	}
	if err := rig.engine.Shutdown(t.Context()); err != nil {
		if !fault.HasCode(err, turn.ErrorCodeRuntimeInternal) {
			t.Fatalf("Shutdown: %v", err)
		}
	}
	for _, req := range []*turn.TurnRequest{turnA, turnB} {
		if terminals := awaitTerminals(t, rig.db, req.TurnID); len(terminals) != 1 {
			t.Fatalf("terminal events of %s = %v, want exactly one", req.TurnID, terminals)
		}
		waitRunEnded(t, rig.runtime, req.TurnID)
	}
}

func TestRecoverTurnsEndsATurnWhoseRunEndedWithoutATerminalEvent(t *testing.T) {
	cases := []struct {
		name     string
		endRun   func(t *testing.T, rig *durableHostRig, req *turn.TurnRequest)
		terminal string
		code     turn.ErrorCode
	}{
		{
			name: "cancelled",
			endRun: func(t *testing.T, rig *durableHostRig, req *turn.TurnRequest) {
				startTurnRun(t, rig.runtime, req)
				if err := rig.runtime.Cancel(t.Context(), durable.RunRef{Key: req.TurnID}); err != nil {
					t.Fatalf("cancel the run: %v", err)
				}
			},
			terminal: turn.EventKindTurnCancelled,
		},
		{
			name: "failed",
			endRun: func(t *testing.T, rig *durableHostRig, req *turn.TurnRequest) {
				failing, err := sqlite.New(&sqlite.Options{DB: rig.db, Logger: slog.New(slog.DiscardHandler), PollInterval: hostRigPoll})
				if err != nil {
					t.Fatalf("sqlite.New: %v", err)
				}
				failing.RegisterHandler("turn", func(context.Context, durable.Invocation) error {
					return errors.New("turn handler refused the run")
				})
				startTurnRun(t, failing, req)
				ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
				hosted := make(chan error, 1)
				go func() { hosted <- failing.Run(ctx) }()
				state := waitRunEnded(t, failing, req.TurnID)
				cancel()
				if err := <-hosted; err != nil {
					t.Fatalf("failing host: %v", err)
				}
				if state != durable.RunFailed {
					t.Fatalf("run ended %s, want failed", state)
				}
			},
			terminal: turn.EventKindTurnFailed,
			code:     turn.ErrorCodeRuntimeInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := newHoldingExecutor()
			rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
			mustCreateSession(t, rig.db, "session-a")
			ended := sampleRequest("session-a", "turn-1")
			admitThroughStore(t, rig.sessions, ended)
			admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-2"))
			tc.endRun(t, rig, ended)
			rig.startHost()

			if err := rig.engine.RecoverTurns(t.Context()); err != nil {
				t.Fatalf("RecoverTurns: %v", err)
			}
			requireTerminals(t, rig.db, eventFilter{turnID: "turn-2", kinds: []string{turn.EventKindTurnCompleted}})
			requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{tc.terminal}})
			if tc.code != "" {
				events, err := store.NewDedupeStore(rig.db).ListTurnEvents(t.Context(), "session-a", "turn-1")
				if err != nil {
					t.Fatalf("list events: %v", err)
				}
				var payload struct {
					Code turn.ErrorCode `json:"code"`
				}
				if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload.Code != tc.code {
					t.Fatalf("terminal payload %s, want code %s", events[len(events)-1].Payload, tc.code)
				}
			}
			if starts := executor.startCount("turn-1"); starts != 0 {
				t.Fatalf("executor started turn-1 %d times, want never", starts)
			}
		})
	}
}

func TestRecoverTurnsGrantsAKnownTurnTheStoreMadeActive(t *testing.T) {
	executor := newHoldingExecutor()
	gate := executor.hold("turn-0")
	engine, sessions, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	engine.statusPoll = hostRigPoll
	mustCreateSession(t, db, "session-a")

	first := callTurn(context.WithoutCancel(t.Context()), engine, sampleRequest("session-a", "turn-0"))
	executor.waitEntered(t, "turn-0")
	second := callTurn(context.WithoutCancel(t.Context()), engine, sampleRequest("session-a", "turn-1"))
	eventually(t, "turn-1 waits in the queue", func() bool {
		_, queued := engine.SessionActivity("session-a")
		return queued == 1
	})
	var statusFails atomic.Bool
	statusFails.Store(true)
	sessions.hooks(func(s *hookedSessionStore) {
		s.aroundStatus = func(_ string, status func() (runtimesessions.StatusResult, error)) (runtimesessions.StatusResult, error) {
			if statusFails.Load() {
				return runtimesessions.StatusResult{}, errors.New("session status unavailable")
			}
			return status()
		}
	})
	close(gate)
	awaitTerminal(t, first, turn.EventKindTurnCompleted)
	eventually(t, "turn-0 leaves the engine", func() bool { return !engineKnows(engine, "turn-0") })
	if active, _ := storeActiveTurn(t, sessions, "session-a"); active != "turn-1" {
		t.Fatalf("store active turn = %q, want turn-1", active)
	}
	if starts := executor.startCount("turn-1"); starts != 0 {
		t.Fatalf("turn-1 started %d times before recovery, want never", starts)
	}

	statusFails.Store(false)
	if err := engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns: %v", err)
	}
	awaitTerminal(t, second, turn.EventKindTurnCompleted)
}

func TestRecoverTurnsIsANoOpWithoutADurableStoreAndAfterShutdown(t *testing.T) {
	plain, _, _ := newTestRuntime(t, Config{}, turntest.NewFakeExecutor(nil))
	if err := plain.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns without a durable store: %v", err)
	}

	executor := newHoldingExecutor()
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()
	if err := rig.engine.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))
	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns after Shutdown: %v", err)
	}
	if active, queued := rig.engine.SessionActivity("session-a"); active != "" || queued != 0 {
		t.Fatalf("SessionActivity = %q, %d; want an engine that holds nothing", active, queued)
	}
	if active, _ := storeActiveTurn(t, rig.sessions, "session-a"); active != "turn-1" {
		t.Fatalf("store active turn = %q, want turn-1", active)
	}
	if starts := executor.startCount("turn-1"); starts != 0 {
		t.Fatalf("executor started turn-1 %d times, want never", starts)
	}
}

func TestRecoverTurnsReportsWhatItCouldNotStage(t *testing.T) {
	executor := newHoldingExecutor()
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	for _, session := range []string{"session-a", "session-b"} {
		mustCreateSession(t, rig.db, session)
	}
	rig.startHost()
	rig.sessions.hooks(func(s *hookedSessionStore) {
		s.aroundActive = func(func() ([]runtimesessions.QueuedTurn, error)) ([]runtimesessions.QueuedTurn, error) {
			return nil, errors.New("listing unavailable")
		}
	})
	err := rig.engine.RecoverTurns(t.Context())
	if !fault.HasCode(err, turn.ErrorCodeStorageUnavailable) {
		t.Fatalf("RecoverTurns with a failing listing = %v, want storage_unavailable", err)
	}

	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-a"))
	ghost := runtimesessions.QueuedTurn{
		Descriptor: turnDescriptorFromRequest(sampleRequest("session-b", "turn-ghost")),
		Sequence:   1,
	}
	rig.sessions.hooks(func(s *hookedSessionStore) {
		s.aroundActive = func(list func() ([]runtimesessions.QueuedTurn, error)) ([]runtimesessions.QueuedTurn, error) {
			listed, err := list()
			if err != nil {
				return nil, err
			}
			return append([]runtimesessions.QueuedTurn{ghost}, listed...), nil
		}
	})
	err = rig.engine.RecoverTurns(t.Context())
	if err == nil || !strings.Contains(err.Error(), "turn-ghost") {
		t.Fatalf("RecoverTurns = %v, want an error naming turn-ghost", err)
	}
	if strings.Contains(err.Error(), "turn-a") {
		t.Fatalf("RecoverTurns = %v, want turn-a recovered", err)
	}
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-a", kinds: []string{turn.EventKindTurnCompleted}})
}

func TestTurnRunIsRefusedWhenItsSessionNoLongerHoldsIt(t *testing.T) {
	cases := []struct {
		name    string
		drop    func(t *testing.T, rig *durableHostRig)
		written []string
	}{
		{
			name: "aborted",
			drop: func(t *testing.T, rig *durableHostRig) {
				if _, err := rig.sessions.Abort(t.Context(), "session-a"); err != nil {
					t.Fatalf("abort: %v", err)
				}
			},
			written: []string{turn.EventKindTurnAccepted, turn.EventKindTurnCancelled},
		},
		{
			name: "erased",
			drop: func(t *testing.T, rig *durableHostRig) {
				if _, err := rig.db.ExecContext(t.Context(), `DELETE FROM session WHERE id = 'session-a'`); err != nil {
					t.Fatalf("erase the session: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := newHoldingExecutor()
			rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
			mustCreateSession(t, rig.db, "session-a")
			req := sampleRequest("session-a", "turn-1")
			admitThroughStore(t, rig.sessions, req)
			startTurnRun(t, rig.runtime, req)
			tc.drop(t, rig)
			rig.startHost()

			if state := waitRunEnded(t, rig.runtime, "turn-1"); state != durable.RunFailed {
				t.Fatalf("run of turn-1 = %s, want failed", state)
			}
			if got := eventKinds(t, rig.db, eventFilter{turnID: "turn-1"}); !slices.Equal(got, tc.written) {
				t.Fatalf("events of turn-1 = %v, want %v", got, tc.written)
			}
			if starts := executor.startCount("turn-1"); starts != 0 {
				t.Fatalf("executor started turn-1 %d times, want never", starts)
			}
		})
	}
}

func TestTurnRunWithATerminalEventOnlyReleases(t *testing.T) {
	for _, released := range []bool{false, true} {
		name := "still held"
		if released {
			name = "already released"
		}
		t.Run(name, func(t *testing.T) {
			executor := newHoldingExecutor()
			rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
			mustCreateSession(t, rig.db, "session-a")
			done := sampleRequest("session-a", "turn-1")
			admitThroughStore(t, rig.sessions, done)
			admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-2"))
			completed := &store.RuntimeEvent{
				ID:            "turn-1-done",
				SessionID:     "session-a",
				TurnID:        "turn-1",
				Author:        "user-1",
				Kind:          turn.EventKindTurnCompleted,
				SchemaVersion: 1,
				Payload:       []byte(`{"outcome":"completed"}`),
				CreatedAt:     time.Now().UTC(),
			}
			if _, err := store.NewEventStore(rig.db).AppendSequenced(t.Context(), "session-a", completed); err != nil {
				t.Fatalf("write turn-1 completed: %v", err)
			}
			startTurnRun(t, rig.runtime, done)
			if released {
				if _, err := rig.sessions.Release(t.Context(), "session-a", "turn-1"); err != nil {
					t.Fatalf("release turn-1: %v", err)
				}
			}
			rig.startHost()

			if state := waitRunEnded(t, rig.runtime, "turn-1"); state != durable.RunSucceeded {
				t.Fatalf("run of turn-1 = %s, want succeeded", state)
			}
			requireTerminals(t, rig.db, eventFilter{turnID: "turn-2", kinds: []string{turn.EventKindTurnCompleted}})
			requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
			if starts := executor.startCount("turn-1"); starts != 0 {
				t.Fatalf("executor started turn-1 %d times, want never", starts)
			}
		})
	}
}

type gateViolations struct {
	mu   sync.Mutex
	seen []string
}

func (v *gateViolations) add(format string, args ...any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, fmt.Sprintf(format, args...))
}

func (v *gateViolations) list() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.seen)
}

func TestQueuedTurnStartsThroughTheGate(t *testing.T) {
	executor := newHoldingExecutor()
	gate := executor.hold("turn-a1")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 16}, executor)
	rig.engine.statusPoll = 200 * time.Millisecond
	sessionOf := map[string]string{"turn-a1": "session-a", "turn-a2": "session-a", "turn-b1": "session-b"}
	violations := &gateViolations{}
	executor.setOnEnter(func(turnID string, othersInside int) {
		if othersInside > 0 {
			violations.add("%s entered while %d other turns were inside", turnID, othersInside)
		}
		if active, _ := rig.engine.SessionActivity(sessionOf[turnID]); active != turnID {
			violations.add("%s entered while its session reported %q active", turnID, active)
		}
	})
	for _, session := range []string{"session-a", "session-b"} {
		mustCreateSession(t, rig.db, session)
	}
	rig.startHost()

	a1 := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-a1"))
	executor.waitEntered(t, "turn-a1")
	a2 := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-a2"))
	eventually(t, "turn-a2 waits in its session", func() bool {
		_, queued := rig.engine.SessionActivity("session-a")
		return queued == 1
	})
	b1 := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-b", "turn-b1"))
	eventually(t, "turn-b1 waits at the gate", func() bool {
		_, queued := rig.engine.SessionActivity("session-b")
		return queued == 1
	})
	close(gate)
	for _, outcome := range []<-chan turnOutcome{a1, a2, b1} {
		awaitTerminal(t, outcome, turn.EventKindTurnCompleted)
	}
	if seen := violations.list(); len(seen) != 0 {
		t.Fatalf("gate violations: %v", seen)
	}
}

func TestPendingCountIsNotConsumedByTurnsTheEngineStaged(t *testing.T) {
	executor := newHoldingExecutor()
	gate := executor.hold("turn-0")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	var mu sync.Mutex
	var seen []int
	executor.setOnEnter(func(string, int) {
		_, _, pending := engineCounts(rig.engine)
		mu.Lock()
		seen = append(seen, pending)
		mu.Unlock()
	})
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()

	submitted := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-0"))
	executor.waitEntered(t, "turn-0")
	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))
	admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-2"))
	close(gate)
	awaitTerminal(t, submitted, turn.EventKindTurnCompleted)
	for _, turnID := range []string{"turn-1", "turn-2"} {
		requireTerminals(t, rig.db, eventFilter{turnID: turnID, kinds: []string{turn.EventKindTurnCompleted}})
	}
	eventually(t, "the engine holds no turn", func() bool {
		active, staged, _ := engineCounts(rig.engine)
		return active == 0 && staged == 0
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || slices.Min(seen) < 0 {
		t.Fatalf("pending counts on entry = %v, want three non-negative counts", seen)
	}
	if _, _, pending := engineCounts(rig.engine); pending != 0 {
		t.Fatalf("pending count after all turns = %d, want 0", pending)
	}
}

func TestTurnGrantedBeforeItIsStagedReachesItsCallerOnADurableRuntime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{
			{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
			{Kind: turn.EventKindMessageCompleted, Payload: []byte(`{}`)},
		})
		engine, sessions, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, db, "session-a")
		sessions.hooks(func(s *hookedSessionStore) {
			s.afterAdmit = func(turnID string) {
				if turnID != "turn-1" {
					return
				}
				close(gate)
				synctest.Wait()
				time.Sleep(2 * durableStatusPoll)
				synctest.Wait()
			}
		})
		first := callTurn(context.WithoutCancel(t.Context()), engine, sampleRequest("session-a", "turn-0"))
		waitFor(t, func() bool { return executor.StartCount() == 1 })
		second := callTurn(context.WithoutCancel(t.Context()), engine, sampleRequest("session-a", "turn-1"))
		awaitTerminal(t, second, turn.EventKindTurnCompleted)
		awaitTerminal(t, first, turn.EventKindTurnCompleted)
		waitFor(t, func() bool { return !engineKnows(engine, "turn-0") && !engineKnows(engine, "turn-1") })
		if starts := executor.StartOrder(); !slices.Equal(starts, []string{"turn-0", "turn-1"}) {
			t.Fatalf("executor starts = %v, want each turn once", starts)
		}
	})
}

func TestRepeatedReleaseKeepsTheSessionKnown(t *testing.T) {
	executor := newHoldingExecutor()
	executor.hold("turn-a")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 1, MaxPendingTurns: 16, ShutdownTimeout: 200 * time.Millisecond}, executor)
	for _, session := range []string{"session-a", "session-b"} {
		mustCreateSession(t, rig.db, session)
	}
	rig.startHost()
	callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-a"))
	executor.waitEntered(t, "turn-a")
	waiting := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-b", "turn-b"))
	eventually(t, "turn-b is granted and waits at the gate", func() bool {
		_, queued := rig.engine.SessionActivity("session-b")
		active, _ := storeActiveTurn(t, rig.sessions, "session-b")
		return queued == 1 && active == "turn-b"
	})

	rig.engine.releaseTurn(t.Context(), &turnState{req: turn.TurnRequest{SessionID: "session-b"}, turnID: "turn-not-held"})
	if err := rig.engine.Shutdown(t.Context()); err != nil {
		if !fault.HasCode(err, turn.ErrorCodeRuntimeInternal) {
			t.Fatalf("Shutdown: %v", err)
		}
	}
	awaitTerminal(t, waiting, turn.EventKindTurnCancelled)
	if active, queued := storeActiveTurn(t, rig.sessions, "session-b"); active != "" || queued != 0 {
		t.Fatalf("session-b holds %q and %d queued after Shutdown, want nothing", active, queued)
	}
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-b", kinds: []string{turn.EventKindTurnCancelled}})
}

func TestTurnAdmittedDuringShutdownLeavesNoRow(t *testing.T) {
	executor := newHoldingExecutor()
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()
	shutdown := make(chan error, 1)
	rig.sessions.hooks(func(s *hookedSessionStore) {
		s.afterAdmit = func(turnID string) {
			if turnID == "turn-1" {
				shutdown <- rig.engine.Shutdown(context.WithoutCancel(t.Context()))
			}
		}
	})
	awaitTerminal(t, callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-1")), turn.EventKindTurnCancelled)
	if err := <-shutdown; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if active, queued := storeActiveTurn(t, rig.sessions, "session-a"); active != "" || queued != 0 {
		t.Fatalf("session-a holds %q and %d queued, want nothing", active, queued)
	}
	if starts := executor.startCount("turn-1"); starts != 0 {
		t.Fatalf("executor started turn-1 %d times, want never", starts)
	}
}

type heldTurnStop struct {
	gate      chan struct{}
	endedTurn string
}

func stopEngineReleasingHeldTurn(t *testing.T, rig *durableHostRig, stop heldTurnStop) {
	t.Helper()
	engine := rig.engine
	stopped := make(chan error, 1)
	go func() { stopped <- engine.Shutdown(context.WithoutCancel(t.Context())) }()
	awaitTerminals(t, rig.db, stop.endedTurn)
	close(stop.gate)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(hostRigTimeout):
		t.Fatal("Shutdown did not return")
	}
}

func TestShutdownCancelsTheQueuedTurnsOfAnEarlierProcess(t *testing.T) {
	t.Run("turns an earlier process admitted", func(t *testing.T) {
		executor := newHoldingExecutor()
		gate := executor.hold("turn-1")
		rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, ShutdownTimeout: 5 * time.Second}, executor)
		mustCreateSession(t, rig.db, "session-a")
		rig.startHost()
		admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))
		startTurnRun(t, rig.runtime, sampleRequest("session-a", "turn-1"))
		admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-2"))
		executor.waitEntered(t, "turn-1")
		if engineKnows(rig.engine, "turn-1") || engineKnows(rig.engine, "turn-2") {
			t.Fatal("engine knows the turns of the earlier process before recovery")
		}

		if err := rig.engine.RecoverTurns(t.Context()); err != nil {
			t.Fatalf("RecoverTurns: %v", err)
		}
		if !engineKnows(rig.engine, "turn-1") || engineKnows(rig.engine, "turn-2") {
			t.Fatal("recovery must attach the active turn-1 and leave the queued turn-2 unknown")
		}
		stopEngineReleasingHeldTurn(t, rig, heldTurnStop{gate: gate, endedTurn: "turn-2"})

		requireTerminals(t, rig.db, eventFilter{turnID: "turn-2", kinds: []string{turn.EventKindTurnCancelled}})
		requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
		if active, queued := storeActiveTurn(t, rig.sessions, "session-a"); active != "" || queued != 0 {
			t.Fatalf("session-a holds %q and %d queued after Shutdown, want nothing", active, queued)
		}
		if starts := executor.startCount("turn-2"); starts != 0 {
			t.Fatalf("executor started turn-2 %d times, want never", starts)
		}
	})

	t.Run("a turn the engine queued", func(t *testing.T) {
		executor := newHoldingExecutor()
		gate := executor.hold("turn-1")
		rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, ShutdownTimeout: 5 * time.Second}, executor)
		mustCreateSession(t, rig.db, "session-a")
		rig.startHost()
		first := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-1"))
		executor.waitEntered(t, "turn-1")
		second := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-2"))
		eventually(t, "the engine queues turn-2", func() bool {
			_, staged, _ := engineCounts(rig.engine)
			return staged == 1
		})

		stopEngineReleasingHeldTurn(t, rig, heldTurnStop{gate: gate, endedTurn: "turn-2"})

		awaitTerminal(t, second, turn.EventKindTurnCancelled)
		awaitTerminal(t, first, turn.EventKindTurnCompleted)
		requireTerminals(t, rig.db, eventFilter{turnID: "turn-2", kinds: []string{turn.EventKindTurnCancelled}})
		requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
		if active, queued := storeActiveTurn(t, rig.sessions, "session-a"); active != "" || queued != 0 {
			t.Fatalf("session-a holds %q and %d queued after Shutdown, want nothing", active, queued)
		}
	})
}

func TestTurnTheEngineAdoptedOutlivesTheCallThatAdoptedIt(t *testing.T) {
	t.Run("recovery call", func(t *testing.T) {
		executor := newHoldingExecutor()
		gate := executor.hold("turn-1")
		rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, rig.db, "session-a")
		rig.startHost()
		admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))
		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		err := rig.engine.RecoverTurns(ctx)
		cancel()
		if err != nil {
			t.Fatalf("RecoverTurns: %v", err)
		}
		eventually(t, "turn-1 enters the executor or ends", func() bool {
			return executor.startCount("turn-1") > 0 || len(terminalsOf(t, rig.db, "turn-1")) > 0
		})
		close(gate)
		requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
	})
	t.Run("caller of the turn before it", func(t *testing.T) {
		executor := newHoldingExecutor()
		before := executor.hold("turn-0")
		gate := executor.hold("turn-1")
		rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
		mustCreateSession(t, rig.db, "session-a")
		rig.startHost()
		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		defer cancel()
		caller := callTurn(ctx, rig.engine, sampleRequest("session-a", "turn-0"))
		executor.waitEntered(t, "turn-0")
		admitThroughStore(t, rig.sessions, sampleRequest("session-a", "turn-1"))
		close(before)
		awaitTerminal(t, caller, turn.EventKindTurnCompleted)
		cancel()
		eventually(t, "turn-1 enters the executor or ends", func() bool {
			return executor.startCount("turn-1") > 0 || len(terminalsOf(t, rig.db, "turn-1")) > 0
		})
		close(gate)
		requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCompleted}})
	})
}

func TestTurnCancelledWhileQueuedEndsCancelled(t *testing.T) {
	executor := newHoldingExecutor()
	gate := executor.hold("turn-0")
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	mustCreateSession(t, rig.db, "session-a")
	rig.startHost()

	running := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-0"))
	executor.waitEntered(t, "turn-0")
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	defer cancel()
	cancelled := callTurn(ctx, rig.engine, sampleRequest("session-a", "turn-1"))
	eventually(t, "turn-1 waits in the queue", func() bool {
		_, queued := rig.engine.SessionActivity("session-a")
		return queued == 1
	})
	third := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-2"))
	eventually(t, "turn-2 waits in the queue", func() bool {
		_, queued := rig.engine.SessionActivity("session-a")
		return queued == 2
	})
	cancel()
	rig.engine.cancelTurn("turn-1")
	close(gate)
	awaitTerminal(t, running, turn.EventKindTurnCompleted)
	awaitTerminal(t, cancelled, turn.EventKindTurnCancelled)
	awaitTerminal(t, third, turn.EventKindTurnCompleted)
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCancelled}})
	if _, err := rig.runtime.Status(t.Context(), durable.RunRef{Key: "turn-1"}); !errors.Is(err, durable.ErrUnknownRun) {
		t.Fatalf("run of turn-1: %v, want no run", err)
	}
	if starts := executor.startCount("turn-1"); starts != 0 {
		t.Fatalf("executor started turn-1 %d times, want never", starts)
	}
}

func TestCancelAfterTheRunCompletedKeepsTheOneTerminalEvent(t *testing.T) {
	executor := newHoldingExecutor()
	engine, _, db := newDurableLiveTestRuntime(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	engine.statusPoll = time.Hour
	mustCreateSession(t, db, "session-a")

	outcomes := callTurn(t.Context(), engine, sampleRequest("session-a", "turn-0"))
	awaitTerminal(t, outcomes, turn.EventKindTurnCompleted)
	if state := waitRunEnded(t, engine.durableRuntime, "turn-0"); state != durable.RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", state)
	}

	cancelledTurn, cancelled, err := engine.CancelActive(t.Context(), "session-a")
	if err != nil || !cancelled || cancelledTurn != "turn-0" {
		t.Fatalf("CancelActive = %q, %v, %v, want the turn its poller still holds", cancelledTurn, cancelled, err)
	}
	eventually(t, "the engine lets go of turn-0", func() bool { return !engineKnows(engine, "turn-0") })
	if got := terminalsOf(t, db, "turn-0"); !slices.Equal(got, []string{turn.EventKindTurnCompleted}) {
		t.Fatalf("terminal events = %v, want the one turn.completed", got)
	}
}

func runUntilCompleted(ctx context.Context, engine *Engine, req *turn.TurnRequest) error {
	for ev, err := range engine.Run(ctx, req) {
		if err != nil {
			return err
		}
		if ev.Kind == turn.EventKindTurnCompleted {
			return nil
		}
	}
	return errors.New("the stream ended before turn.completed")
}

func TestSubmitterLeavingAfterTheTerminalEventDoesNotCancelTheRun(t *testing.T) {
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, newHoldingExecutor())
	mustCreateSession(t, rig.db, "session-a")
	releasing, proceed := make(chan struct{}), make(chan struct{})
	var handlerReleased atomic.Bool
	rig.sessions.hooks(func(s *hookedSessionStore) {
		s.aroundRelease = func(_, _ string, release func() (runtimesessions.ReleaseResult, error)) (runtimesessions.ReleaseResult, error) {
			if handlerReleased.CompareAndSwap(false, true) {
				close(releasing)
				<-proceed
			}
			return release()
		}
	})
	rig.startHost()

	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	if err := runUntilCompleted(ctx, rig.engine, sampleRequest("session-a", "turn-0")); err != nil {
		t.Fatalf("turn stream: %v", err)
	}
	select {
	case <-releasing:
	case <-time.After(hostRigTimeout):
		t.Fatal("the run never reached its release")
	}
	leave()
	eventually(t, "the engine lets go of turn-0", func() bool { return !engineKnows(rig.engine, "turn-0") })
	if active, _ := storeActiveTurn(t, rig.sessions, "session-a"); active != "" {
		t.Fatalf("active turn = %q, want the session released by the engine", active)
	}
	close(proceed)

	if state := waitRunEnded(t, rig.runtime, "turn-0"); state != durable.RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", state)
	}
	if got := terminalsOf(t, rig.db, "turn-0"); !slices.Equal(got, []string{turn.EventKindTurnCompleted}) {
		t.Fatalf("terminal events = %v, want the one turn.completed", got)
	}
}

type budgetedTurnEvents struct {
	DedupeStore

	readsLeft atomic.Int64
}

func (d *budgetedTurnEvents) ListTurnEvents(ctx context.Context, sessionID, turnID string) ([]store.RuntimeEvent, error) {
	if d.readsLeft.Add(-1) < 0 {
		return nil, errors.New("turn events are unreadable")
	}
	return d.DedupeStore.ListTurnEvents(ctx, sessionID, turnID)
}

func TestEndedTurnWhoseEventsCannotBeReadStaysActiveUntilTheNextRecovery(t *testing.T) {
	executor := newHoldingExecutor()
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	events := &budgetedTurnEvents{DedupeStore: rig.engine.dedupe}
	rig.engine.dedupe = events
	mustCreateSession(t, rig.db, "session-a")
	req := sampleRequest("session-a", "turn-1")
	admitThroughStore(t, rig.sessions, req)
	startTurnRun(t, rig.runtime, req)
	if err := rig.runtime.Cancel(t.Context(), durable.RunRef{Key: req.TurnID}); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	if state := waitRunEnded(t, rig.runtime, req.TurnID); state != durable.RunCancelled {
		t.Fatalf("run ended %s, want cancelled", state)
	}
	rig.startHost()

	events.readsLeft.Store(1)
	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns with one readable pass: %v", err)
	}
	eventually(t, "the poller gives up on turn-1", func() bool {
		return events.readsLeft.Load() < 0 && !engineKnows(rig.engine, "turn-1")
	})
	if active, _ := storeActiveTurn(t, rig.sessions, "session-a"); active != "turn-1" {
		t.Fatalf("active turn = %q, want turn-1 still held", active)
	}
	if got := terminalsOf(t, rig.db, "turn-1"); len(got) != 0 {
		t.Fatalf("terminal events = %v, want none while the events are unreadable", got)
	}

	events.readsLeft.Store(1 << 30)
	if err := rig.engine.RecoverTurns(t.Context()); err != nil {
		t.Fatalf("RecoverTurns with readable events: %v", err)
	}
	requireTerminals(t, rig.db, eventFilter{turnID: "turn-1", kinds: []string{turn.EventKindTurnCancelled}})
	eventually(t, "the session lets go of turn-1", func() bool {
		active, _ := storeActiveTurn(t, rig.sessions, "session-a")
		return active == ""
	})
	if starts := executor.startCount("turn-1"); starts != 0 {
		t.Fatalf("executor started turn-1 %d times, want never", starts)
	}
}
