package runtimeengine

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/store"
)

const countedPersona = "persona-a"

type recordingCounter struct {
	mu       sync.Mutex
	personas []string
}

func (c *recordingCounter) fn(_ context.Context, req *turn.TurnRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.personas = append(c.personas, req.AgentID)
}

func (c *recordingCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.personas)
}

func postTurnRecordsOf(t *testing.T, db *sql.DB, turnID string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT CAST(result AS TEXT) FROM durable_action WHERE run_key = ? AND action_key = ?`, turnID, postTurnAction)
	if err != nil {
		t.Fatalf("read the post-turn records of %s: %v", turnID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan a post-turn record of %s: %v", turnID, err)
		}
		out = append(out, result)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the post-turn records of %s: %v", turnID, err)
	}
	return out
}

func requirePostTurnRecord(t *testing.T, db *sql.DB, turnID string) {
	t.Helper()
	if got := postTurnRecordsOf(t, db, turnID); len(got) != 1 || got[0] != `{}` {
		t.Fatalf("post-turn records of %s = %v, want one empty record", turnID, got)
	}
}

func requireCalls(t *testing.T, hook *recordingHook, counter *recordingCounter, calls, counted int) {
	t.Helper()
	hook.mu.Lock()
	gotCalls := hook.calls
	hook.mu.Unlock()
	if gotCalls != calls {
		t.Errorf("post-turn hook calls = %d, want %d", gotCalls, calls)
	}
	if got := counter.count(); got != counted {
		t.Errorf("counted turns = %d, want %d", got, counted)
	}
}

func channelPostTurnRequest(turnID string) *turn.TurnRequest {
	req := channelTurnRequest("channel:333", turnID)
	req.AgentID = countedPersona
	return req
}

func TestDurableTurnRecordsItsPostTurnWork(t *testing.T) {
	run := func(t *testing.T, cfg Config) (*durableHostRig, *lockedBuffer) {
		t.Helper()
		cfg.MaxActiveTurns, cfg.MaxPendingTurns = 4, 16
		rig := newDurableHostRig(t, cfg, turn.NewFakeExecutor([]turn.FakeStep{textStep("hello")}))
		logs := &lockedBuffer{}
		rig.engine.logger = slog.New(slog.NewTextHandler(logs, nil))
		rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{&presenceCapture{}})
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		awaitTerminal(t, callTurn(context.WithoutCancel(t.Context()), rig.engine, channelPostTurnRequest("turn-1")), turn.EventKindTurnCompleted)
		return rig, logs
	}
	t.Run("a completed turn", func(t *testing.T) {
		hook, counter := &recordingHook{}, &recordingCounter{}
		rig, _ := run(t, Config{PostTurnHook: hook.fn, CountTurn: counter.fn})
		requireCalls(t, hook, counter, 1, 1)
		if !slices.Equal(hook.counts, []int{1}) {
			t.Errorf("events passed to the hook = %v, want the turn's one event", hook.counts)
		}
		if !slices.Equal(counter.personas, []string{countedPersona}) {
			t.Errorf("counted personas = %v, want %s", counter.personas, countedPersona)
		}
		requirePostTurnRecord(t, rig.db, "turn-1")
	})
	t.Run("a hook that fails", func(t *testing.T) {
		hook, counter := &recordingHook{err: errors.New("boom hook")}, &recordingCounter{}
		rig, logs := run(t, Config{PostTurnHook: hook.fn, CountTurn: counter.fn})
		requireCalls(t, hook, counter, 1, 1)
		requirePostTurnRecord(t, rig.db, "turn-1")
		if got := strings.Count(logs.String(), "post-turn hook failed"); got != 1 {
			t.Errorf("the failed hook was logged %d times, want once: %q", got, logs.String())
		}
	})
	t.Run("an engine without post-turn work", func(t *testing.T) {
		rig, _ := run(t, Config{})
		if got := postTurnRecordsOf(t, rig.db, "turn-1"); len(got) != 0 {
			t.Fatalf("post-turn records = %v, want none", got)
		}
	})
}

type crashAtPresenceEnd struct {
	*presenceCapture
	crash *crashAfterDelivery
}

func (a *crashAtPresenceEnd) PresenceEnded(ctx context.Context, turnID string) error {
	a.crash.handOff(ctx, turnID)
	return a.presenceCapture.PresenceEnded(ctx, turnID)
}

type twoLives struct {
	cfg      Config
	adapter  runtimechannelhost.ChannelPort
	presence *presenceCapture
	crash    *crashAfterDelivery
	endsOne  []string
}

func (l *twoLives) run(t *testing.T) *durableHostRig {
	t.Helper()
	db := l.crash.db
	l.cfg.MaxActiveTurns, l.cfg.MaxPendingTurns = 4, 16
	rig := &durableHostRig{t: t, cfg: l.cfg, db: db, sessions: newHookedSessionStore(t, db)}
	executor := turn.NewFakeExecutor([]turn.FakeStep{textStep("hello once")})
	rig.build(executor)
	rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{l.adapter})
	mustCreateSession(t, db, "channel:333")
	rig.startHost()
	_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelPostTurnRequest("turn-1"))
	l.crash.await(t)
	rig.stopHost()
	if terminals := terminalsOf(t, db, "turn-1"); len(terminals) != 0 {
		t.Fatalf("terminal events after the first life = %v, want none", terminals)
	}
	_, _, l.endsOne = l.presence.counts()
	if _, err := db.ExecContext(t.Context(), `DELETE FROM durable_host WHERE owner = ?`, ghostHostOwner); err != nil {
		t.Fatalf("drop the other host: %v", err)
	}
	rig.build(executor)
	rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{l.adapter})
	rig.startHost()
	awaitTerminals(t, db, "turn-1")
	return rig
}

func newCrashingLives(t *testing.T) (*twoLives, *crashAfterDelivery) {
	t.Helper()
	crash := &crashAfterDelivery{db: openTestDatabase(t), crashed: make(chan struct{})}
	presence := &presenceCapture{}
	return &twoLives{presence: presence, adapter: presence, crash: crash}, crash
}

func TestReplayedTurnDoesNotRepeatItsPostTurnWork(t *testing.T) {
	t.Run("a crash after the record", func(t *testing.T) {
		lives, crash := newCrashingLives(t)
		hook, counter := &recordingHook{}, &recordingCounter{}
		lives.cfg = Config{PostTurnHook: hook.fn, CountTurn: counter.fn}
		lives.adapter = &crashAtPresenceEnd{presenceCapture: lives.presence, crash: crash}
		rig := lives.run(t)
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		requireCalls(t, hook, counter, 1, 1)
		requirePostTurnRecord(t, rig.db, "turn-1")
	})
	t.Run("a crash inside the hook", func(t *testing.T) {
		lives, crash := newCrashingLives(t)
		hook, counter := &recordingHook{}, &recordingCounter{}
		lives.cfg = Config{
			PostTurnHook: func(ctx context.Context, req *turn.TurnRequest, events []store.RuntimeEvent) error {
				_ = hook.fn(ctx, req, events)
				return crash.hook(ctx, req, events)
			},
			CountTurn: counter.fn,
		}
		rig := lives.run(t)
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		requireCalls(t, hook, counter, 2, 1)
		requirePostTurnRecord(t, rig.db, "turn-1")
		if len(lives.endsOne) != 0 {
			t.Errorf("presence ends after the first life = %v, want none", lives.endsOne)
		}
		if _, _, ends := lives.presence.counts(); len(ends) != 1 {
			t.Errorf("presence ends across both lives = %v, want one", ends)
		}
	})
}

type handingHook struct {
	recordingHook
	crash *crashAfterDelivery
}

func (h *handingHook) fn(ctx context.Context, req *turn.TurnRequest, events []store.RuntimeEvent) error {
	_ = h.recordingHook.fn(ctx, req, events)
	return h.crash.hook(ctx, req, events)
}

func TestCancelledDurableTurnDoesItsPostTurnWork(t *testing.T) {
	start := func(t *testing.T, cfg Config) (rig *durableHostRig, presence *presenceCapture, outcome <-chan turnOutcome, logs *lockedBuffer) {
		t.Helper()
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		executor := turn.NewFakeExecutor([]turn.FakeStep{textStep("partial answer"), {Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
		cfg.MaxActiveTurns, cfg.MaxPendingTurns = 4, 16
		rig = newDurableHostRig(t, cfg, executor)
		logs = &lockedBuffer{}
		rig.engine.logger = slog.New(slog.NewTextHandler(logs, nil))
		presence = &presenceCapture{}
		rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{presence})
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		outcome = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelPostTurnRequest("turn-1"))
		eventually(t, "the turn reaches the executor", func() bool { return executor.StartCount() == 1 })
		eventually(t, "the model message is stored", func() bool { return len(executorEventsOf(t, rig.db, "turn-1")) == 1 })
		return rig, presence, outcome, logs
	}
	cancel := func(t *testing.T, rig *durableHostRig) {
		t.Helper()
		if _, cancelled, err := rig.engine.CancelActive(t.Context(), "channel:333"); err != nil || !cancelled {
			t.Fatalf("CancelActive = %v, %v; want the turn cancelled", cancelled, err)
		}
	}
	t.Run("a cancelled turn", func(t *testing.T) {
		hook, counter := &recordingHook{}, &recordingCounter{}
		rig, presence, outcome, _ := start(t, Config{PostTurnHook: hook.fn, CountTurn: counter.fn})
		cancel(t, rig)
		awaitTerminal(t, outcome, turn.EventKindTurnCancelled)
		requireCalls(t, hook, counter, 1, 1)
		if !slices.Equal(hook.counts, []int{1}) {
			t.Errorf("events passed to the hook = %v, want the one stored before the cancel", hook.counts)
		}
		requirePostTurnRecord(t, rig.db, "turn-1")
		if _, failed, ended := presence.counts(); failed != 1 || len(ended) != 1 {
			t.Errorf("failed presence updates = %d, presence ends = %v; want 1 and one", failed, ended)
		}
	})
	t.Run("a cancelled turn whose record is fenced", func(t *testing.T) {
		counter := &recordingCounter{}
		hook := &handingHook{crash: &crashAfterDelivery{crashed: make(chan struct{})}}
		rig, _, outcome, logs := start(t, Config{PostTurnHook: hook.fn, CountTurn: counter.fn})
		hook.crash.db = rig.db
		cancel(t, rig)
		awaitTerminal(t, outcome, turn.EventKindTurnCancelled)
		hook.crash.await(t)
		if got := strings.Count(logs.String(), "post-turn record failed"); got != 1 || !strings.Contains(logs.String(), "turn-1") {
			t.Errorf("the failed record was logged %d times, want once with the turn id: %q", got, logs.String())
		}
		if got := postTurnRecordsOf(t, rig.db, "turn-1"); len(got) != 0 {
			t.Errorf("post-turn records = %v, want none", got)
		}
		if got := counter.count(); got != 0 {
			t.Errorf("counted turns = %d, want 0", got)
		}
	})
}

func TestPreemptedDurableTurnDoesItsPostTurnWorkOnce(t *testing.T) {
	cases := []struct {
		name    string
		rig     func(*testing.T, Config, TurnExecutor) *durableHostRig
		preempt func(*testing.T, *durableHostRig, TurnExecutor)
	}{
		{
			name: "a host stop",
			rig:  newDurableHostRig,
			preempt: func(t *testing.T, rig *durableHostRig, executor TurnExecutor) {
				t.Helper()
				rig.stopHost()
				rig.build(executor)
				rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{&presenceCapture{}})
				rig.startHost()
			},
		},
		{
			name: "a lost lease",
			rig:  newShortLeaseHostRig,
			preempt: func(t *testing.T, rig *durableHostRig, _ TurnExecutor) {
				t.Helper()
				loseHostLease(t, rig.db)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook, counter := &recordingHook{}, &recordingCounter{}
			executor := newTwoInvocationExecutor()
			rig := tc.rig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, PostTurnHook: hook.fn, CountTurn: counter.fn}, executor)
			rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{&presenceCapture{}})
			mustCreateSession(t, rig.db, "channel:333")
			rig.startHost()
			_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelPostTurnRequest("turn-1"))
			executor.waitBlocked(t)
			tc.preempt(t, rig, executor)
			awaitTerminals(t, rig.db, "turn-1")
			if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
				t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
			}
			requireCalls(t, hook, counter, 1, 1)
			if !slices.Equal(hook.counts, []int{1}) {
				t.Errorf("events passed to the hook = %v, want the resumed invocation's one event", hook.counts)
			}
			requirePostTurnRecord(t, rig.db, "turn-1")
		})
	}
}

func TestTurnIsCountedOnceAfterItsPostTurnHook(t *testing.T) {
	newOrder := func(t *testing.T, executor TurnExecutor) (*Engine, func() []string) {
		t.Helper()
		var mu sync.Mutex
		var order []string
		engine, db, _ := newTestRuntime(t, Config{
			MaxActiveTurns:  2,
			MaxPendingTurns: 4,
			PostTurnHook: func(context.Context, *turn.TurnRequest, []store.RuntimeEvent) error {
				mu.Lock()
				defer mu.Unlock()
				order = append(order, "hook")
				return nil
			},
			CountTurn: func(context.Context, *turn.TurnRequest) {
				mu.Lock()
				defer mu.Unlock()
				order = append(order, "count")
			},
		}, executor)
		mustCreateSession(t, db, "session-a")
		return engine, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(order)
		}
	}
	want := []string{"hook", "count"}
	t.Run("a completed turn", func(t *testing.T) {
		engine, order := newOrder(t, turn.NewFakeExecutor([]turn.FakeStep{hookMessageStep("hello")}))
		if _, err := collect(t, engine, sampleRequest("session-a", "turn-1")); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := order(); !slices.Equal(got, want) {
			t.Fatalf("post-turn calls = %v, want %v", got, want)
		}
	})
	t.Run("a cancelled turn", func(t *testing.T) {
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		executor := turn.NewFakeExecutor([]turn.FakeStep{{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate}})
		engine, order := newOrder(t, executor)
		outcome := callTurn(context.WithoutCancel(t.Context()), engine, sampleRequest("session-a", "turn-1"))
		eventually(t, "the turn reaches the executor", func() bool { return executor.StartCount() == 1 })
		if _, cancelled, err := engine.CancelActive(t.Context(), "session-a"); err != nil || !cancelled {
			t.Fatalf("CancelActive = %v, %v; want the turn cancelled", cancelled, err)
		}
		awaitTerminal(t, outcome, turn.EventKindTurnCancelled)
		if got := order(); !slices.Equal(got, want) {
			t.Fatalf("post-turn calls = %v, want %v", got, want)
		}
	})
}
