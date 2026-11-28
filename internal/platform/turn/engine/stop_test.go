package runtimeengine

import (
	"context"
	"database/sql"
	"iter"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

type engineCancelTurns struct {
	engine *Engine
}

func (t engineCancelTurns) CancelActive(ctx context.Context, sessionID string) (turnID string, cancelled bool, err error) {
	return t.engine.CancelActive(ctx, sessionID)
}

func (t engineCancelTurns) CancelTurn(ctx context.Context, sessionID, turnID string) (bool, error) {
	return t.engine.CancelTurn(ctx, sessionID, turnID)
}

func (t engineCancelTurns) Activity(sessionID string) (activeTurnID string, queued int) {
	return t.engine.SessionActivity(sessionID)
}

type noopCommandSessions struct{}

func (noopCommandSessions) EventCount(context.Context, string) (int, error) { return 0, nil }

func (noopCommandSessions) Create(context.Context, *command.Caller) (command.Session, error) {
	return command.Session{}, nil
}

func (noopCommandSessions) Switch(context.Context, *command.Caller, string) (command.Session, error) {
	return command.Session{}, nil
}

func engineCommandDispatcher(t *testing.T, engine *Engine) *command.Dispatcher {
	t.Helper()
	registry, err := command.New(command.Binding{
		Channel: principal.ChannelTelegram,
		Style:   command.StyleMessenger,
		Names:   command.MessengerNames(),
	})
	if err != nil {
		t.Fatalf("command.New: %v", err)
	}
	dispatcher, err := command.NewDispatcher(registry, &command.Ports{
		Sessions: noopCommandSessions{},
		Turns:    engineCancelTurns{engine: engine},
	}, nil)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher
}

func ownerChannelTurnRequest(sessionID, turnID string, class principal.Class) *turn.TurnRequest {
	req := channelTurnRequest(sessionID, turnID)
	req.PrincipalClass = class
	return req
}

func engineModes() map[string]func(t *testing.T, deps Deps, executor TurnExecutor) (*Engine, *sql.DB) {
	return map[string]func(t *testing.T, deps Deps, executor TurnExecutor) (*Engine, *sql.DB){
		"in-process": func(t *testing.T, deps Deps, executor TurnExecutor) (*Engine, *sql.DB) {
			engine, db, _ := newTestRuntime(t, deps, executor)
			return engine, db
		},
		"durable": func(t *testing.T, deps Deps, executor TurnExecutor) (*Engine, *sql.DB) {
			engine, _, db := newDurableLiveTestRuntime(t, deps, executor)
			return engine, db
		},
	}
}

func TestStopPressCancelsExactlyThatTurn(t *testing.T) {
	for mode, build := range engineModes() {
		t.Run(mode, func(t *testing.T) {
			gate := make(chan struct{})
			t.Cleanup(func() { close(gate) })
			executor := turntest.NewFakeExecutor([]turntest.FakeStep{
				{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
			})
			capture := &presenceCapture{}
			engine, db := build(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
			mustCreateSession(t, db, "channel:333")
			dispatcher := engineCommandDispatcher(t, engine)

			outcome := callTurn(context.WithoutCancel(t.Context()), engine, ownerChannelTurnRequest("channel:333", "turn-stop-1", principal.ClassOwner))
			waitFor(t, func() bool { return executor.StartCount() == 1 })

			cancelled, err := dispatcher.CancelTurn(t.Context(), "channel:333", "turn-stop-1")
			if err != nil || !cancelled {
				t.Fatalf("CancelTurn = %v, %v; want true, nil", cancelled, err)
			}
			awaitTerminal(t, outcome, turn.EventKindTurnCancelled)

			capture.mu.Lock()
			ends := append([]runtimechannelhost.PresenceEnd(nil), capture.endOutcomes...)
			ended := append([]string(nil), capture.ended...)
			delivers := capture.delivers
			capture.mu.Unlock()
			if delivers != 0 {
				t.Errorf("delivers = %d, want 0", delivers)
			}
			if len(ended) != 1 || ended[0] != "turn-stop-1" || len(ends) != 1 || !ends[0].Stopped {
				t.Fatalf("presence ends = %v %+v, want one stopped end for turn-stop-1", ended, ends)
			}
		})
	}
}

func TestChannelTurnStopTokenExpiresAtTheTurnDeadline(t *testing.T) {
	for mode, build := range engineModes() {
		t.Run(mode, func(t *testing.T) {
			limits := testLimits(2, 4)
			limits.TurnTimeout = configkit.Duration(90 * time.Second)
			executor := turntest.NewFakeExecutor([]turntest.FakeStep{textStep("done")})
			capture := &presenceCapture{}
			engine, db := build(t, Deps{Config: limits, Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
			mustCreateSession(t, db, "channel:333")

			before := time.Now().UTC()
			req := ownerChannelTurnRequest("channel:333", "turn-deadline-1", principal.ClassOwner)
			req.Deadline = time.Time{}
			if _, err := collect(t, engine, req); err != nil {
				t.Fatalf("Run: %v", err)
			}
			after := time.Now().UTC()

			capture.mu.Lock()
			started := append([]*runtimechannelhost.DeliveryRequest(nil), capture.startedReqs...)
			capture.mu.Unlock()
			if len(started) != 1 {
				t.Fatalf("started requests = %d, want 1", len(started))
			}
			got := started[0]
			if got.PrincipalClass != string(principal.ClassOwner) {
				t.Errorf("PrincipalClass = %q, want %q", got.PrincipalClass, principal.ClassOwner)
			}
			if got.Deadline.IsZero() {
				t.Fatal("computed presence deadline is zero")
			}
			minWant := before.Add(time.Duration(limits.TurnTimeout) - 2*time.Second)
			maxWant := after.Add(time.Duration(limits.TurnTimeout) + 2*time.Second)
			if got.Deadline.Before(minWant) || got.Deadline.After(maxWant) {
				t.Errorf("Deadline = %v, want between %v and %v", got.Deadline, minWant, maxWant)
			}
		})
	}
}

func TestLateStopPressDoesNotCancelANewerTurn(t *testing.T) {
	for mode, build := range engineModes() {
		t.Run(mode, func(t *testing.T) {
			firstDone := turntest.NewFakeExecutor([]turntest.FakeStep{textStep("first answer")})
			gate := make(chan struct{})
			secondBlocked := turntest.NewFakeExecutor([]turntest.FakeStep{
				{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
				textStep("second answer"),
			})
			var activeExec TurnExecutor = firstDone
			switching := executorFunc(func(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error] {
				return activeExec.Execute(ctx, req)
			})
			capture := &presenceCapture{}
			engine, db := build(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, switching)
			mustCreateSession(t, db, "channel:333")
			dispatcher := engineCommandDispatcher(t, engine)

			if _, err := collect(t, engine, ownerChannelTurnRequest("channel:333", "turn-old", principal.ClassOwner)); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			waitFor(t, func() bool { active, _ := engine.SessionActivity("channel:333"); return active == "" })

			activeExec = secondBlocked
			secondOutcome := callTurn(context.WithoutCancel(t.Context()), engine, ownerChannelTurnRequest("channel:333", "turn-new", principal.ClassOwner))
			waitFor(t, func() bool { return secondBlocked.StartCount() == 1 })

			cancelled, err := dispatcher.CancelTurn(t.Context(), "channel:333", "turn-old")
			if err != nil || cancelled {
				t.Fatalf("CancelTurn(old) = %v, %v; want false, nil", cancelled, err)
			}
			if active, _ := engine.SessionActivity("channel:333"); active != "turn-new" {
				t.Fatalf("active turn = %q after stale CancelTurn, want turn-new", active)
			}
			if _, err := engine.CancelTurn(t.Context(), "", "turn-new"); err == nil {
				t.Error("CancelTurn accepted empty session id")
			}
			if _, err := engine.CancelTurn(t.Context(), "channel:333", ""); err == nil {
				t.Error("CancelTurn accepted empty turn id")
			}

			close(gate)
			awaitTerminal(t, secondOutcome, turn.EventKindTurnCompleted)
		})
	}
}

func TestStopEndStates(t *testing.T) {
	t.Run("typed cancel of owner and non-owner turns", func(t *testing.T) {
		classes := []struct {
			name  string
			class principal.Class
		}{
			{"owner", principal.ClassOwner},
			{"non-owner", principal.ClassTrusted},
		}
		for mode, build := range engineModes() {
			for _, tc := range classes {
				t.Run(mode+"/"+tc.name, func(t *testing.T) {
					gate := make(chan struct{})
					t.Cleanup(func() { close(gate) })
					executor := turntest.NewFakeExecutor([]turntest.FakeStep{
						{Kind: turn.EventKindModelStarted, Payload: []byte(`{}`), Block: gate},
					})
					capture := &presenceCapture{}
					engine, db := build(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
					mustCreateSession(t, db, "channel:333")

					turnID := "turn-cancel-" + tc.name
					outcome := callTurn(context.WithoutCancel(t.Context()), engine, ownerChannelTurnRequest("channel:333", turnID, tc.class))
					waitFor(t, func() bool { return executor.StartCount() == 1 })

					if _, cancelled, err := engine.CancelActive(t.Context(), "channel:333"); err != nil || !cancelled {
						t.Fatalf("CancelActive = %v, %v; want true, nil", cancelled, err)
					}
					awaitTerminal(t, outcome, turn.EventKindTurnCancelled)

					capture.mu.Lock()
					ends := append([]runtimechannelhost.PresenceEnd(nil), capture.endOutcomes...)
					started := append([]*runtimechannelhost.DeliveryRequest(nil), capture.startedReqs...)
					capture.mu.Unlock()
					if len(started) != 1 || started[0].PrincipalClass != string(tc.class) {
						t.Fatalf("started = %+v, want class %q", started, tc.class)
					}
					if len(ends) != 1 || !ends[0].Stopped {
						t.Fatalf("PresenceEnded outcomes = %+v, want Stopped=true", ends)
					}
				})
			}
		}
	})

	t.Run("preempted durable turn does not report stopped", func(t *testing.T) {
		executor := newTwoInvocationExecutor()
		capture := &presenceCapture{}
		rig := newShortLeaseHostRig(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, ownerChannelTurnRequest("channel:333", "turn-preempt-end", principal.ClassOwner))
		executor.waitBlocked(t)
		loseHostLease(t, rig.db)
		awaitTerminal(t, outcome, turn.EventKindTurnCompleted)

		capture.mu.Lock()
		ends := append([]runtimechannelhost.PresenceEnd(nil), capture.endOutcomes...)
		capture.mu.Unlock()
		if len(ends) != 2 {
			t.Fatalf("PresenceEnded calls = %d, want 2", len(ends))
		}
		for i, end := range ends {
			if end.Stopped {
				t.Errorf("PresenceEnded[%d].Stopped = true, want false on preemption and completion", i)
			}
		}
	})

	t.Run("late cancel during delivery does not mark stopped", func(t *testing.T) {
		for mode, build := range engineModes() {
			t.Run(mode, func(t *testing.T) {
				executor := turntest.NewFakeExecutor([]turntest.FakeStep{textStep("delivered answer")})
				var engine *Engine
				capture := &presenceCapture{}
				capture.onDeliver = func() {
					_, _ = engine.CancelTurn(context.Background(), "channel:333", "turn-late-cancel")
				}
				var db *sql.DB
				engine, db = build(t, Deps{Config: testLimits(4, 16), Channels: []runtimechannelhost.ChannelPort{capture}}, executor)
				mustCreateSession(t, db, "channel:333")

				outcome := callTurn(context.WithoutCancel(t.Context()), engine, ownerChannelTurnRequest("channel:333", "turn-late-cancel", principal.ClassOwner))
				awaitTerminal(t, outcome, turn.EventKindTurnCompleted)

				capture.mu.Lock()
				ends := append([]runtimechannelhost.PresenceEnd(nil), capture.endOutcomes...)
				delivers := capture.delivers
				capture.mu.Unlock()
				if delivers != 1 {
					t.Fatalf("delivers = %d, want 1", delivers)
				}
				if len(ends) != 1 || ends[0].Stopped {
					t.Fatalf("PresenceEnded outcomes = %+v, want Stopped=false after delivery", ends)
				}
			})
		}
	})
}

type executorFunc func(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error]

func (f executorFunc) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error] {
	return f(ctx, req)
}
