package runtimeengine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

type acceptedEvents struct {
	store.EventStore
	onAccepted func(turnID string)
}

func (s *acceptedEvents) Append(ctx context.Context, ev *store.RuntimeEvent) error {
	if err := s.EventStore.Append(ctx, ev); err != nil {
		return err
	}
	if ev.Kind == turn.EventKindTurnAccepted {
		s.onAccepted(ev.TurnID)
	}
	return nil
}

type acceptedDedupe struct {
	store.DedupeStore
	onAccepted func(turnID string)
}

func (s *acceptedDedupe) Accept(ctx context.Context, source, externalID string, expiresAt time.Time, accepted *store.RuntimeEvent) (originalTurnID string, created bool, err error) {
	originalTurnID, created, err = s.DedupeStore.Accept(ctx, source, externalID, expiresAt, accepted)
	if err == nil && created {
		s.onAccepted(accepted.TurnID)
	}
	return originalTurnID, created, err
}

type enqueueGateContext struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func newEnqueueGateContext() *enqueueGateContext {
	return &enqueueGateContext{
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *enqueueGateContext) Done() <-chan struct{} {
	if c.armed.CompareAndSwap(true, false) {
		close(c.reached)
		<-c.release
	}
	return nil
}

func (c *enqueueGateContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *enqueueGateContext) Err() error { return nil }

func (c *enqueueGateContext) Value(any) any { return nil }

func (e *Engine) isQueued(turnID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.turns[turnID]
	return ok
}

func TestConcurrentSubmitsStartInAcceptedOrder(t *testing.T) {
	const settleWindow = 500 * time.Millisecond
	const deadline = 30 * time.Second
	cases := []struct {
		name       string
		idempotent bool
		busy       bool
	}{
		{name: "plain submit on a busy session", busy: true},
		{name: "plain submit on an idle session"},
		{name: "idempotent accept on a busy session", idempotent: true, busy: true},
		{name: "idempotent accept on an idle session", idempotent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := make(chan struct{})
			executor := turn.NewFakeExecutor([]turn.FakeStep{{Kind: turn.EventKindModelStarted, Block: gate}})
			engine, db, events := newTestRuntime(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
			session := sampleSession("conv-1")
			mustCreateSession(t, db, session)

			gated := newEnqueueGateContext()
			var armNext atomic.Bool
			persisted := make(chan string, 8)
			onAccepted := func(turnID string) {
				if armNext.CompareAndSwap(true, false) {
					gated.armed.Store(true)
				}
				persisted <- turnID
			}
			engine.events = &acceptedEvents{EventStore: events, onAccepted: onAccepted}
			engine.dedupe = &acceptedDedupe{DedupeStore: store.NewDedupeStore(db), onAccepted: onAccepted}

			send := func(ctx context.Context, name string) (string, error) {
				if tc.idempotent {
					ref, err := engine.Accept(ctx, sampleEnvelope("conv-1", name))
					return ref.TurnID, err
				}
				sub, err := engine.submit(ctx, sampleRequest(session, name))
				if err != nil {
					return "", err
				}
				sub.stop()
				return name, nil
			}
			type sent struct {
				turnID string
				err    error
			}
			sendAsync := func(ctx context.Context, name string) <-chan sent {
				out := make(chan sent, 1)
				go func() {
					turnID, err := send(ctx, name)
					out <- sent{turnID: turnID, err: err}
				}()
				return out
			}
			nextPersisted := func() string {
				select {
				case turnID := <-persisted:
					return turnID
				case <-time.After(deadline):
					t.Fatal("no accepted event was persisted")
					return ""
				}
			}

			var want []string
			if tc.busy {
				busy := <-sendAsync(context.Background(), "turn-busy")
				if busy.err != nil {
					t.Fatalf("busy send: %v", busy.err)
				}
				nextPersisted()
				waitFor(t, func() bool { return executor.StartCount() == 1 })
				want = append(want, busy.turnID)
			}

			armNext.Store(true)
			first := sendAsync(gated, "turn-a")
			select {
			case <-gated.reached:
			case <-time.After(deadline):
				t.Fatal("the first submit never reached its enqueue")
			}
			firstTurn := nextPersisted()

			second := sendAsync(context.Background(), "turn-b")
			select {
			case secondTurn := <-persisted:
				waitFor(t, func() bool { return engine.isQueued(secondTurn) })
			case <-time.After(settleWindow):
			}
			close(gated.release)

			for _, out := range []<-chan sent{first, second} {
				res := <-out
				if res.err != nil {
					t.Fatalf("send: %v", res.err)
				}
				want = append(want, res.turnID)
			}
			if want[len(want)-2] != firstTurn {
				t.Fatalf("first submit turn = %q, want %q", want[len(want)-2], firstTurn)
			}

			waitFor(t, func() bool { return engine.isQueued(want[len(want)-1]) && engine.isQueued(firstTurn) })
			close(gate)
			for _, turnID := range want {
				waitForTerminalDurable(t, db, turnID)
			}
			if got := executor.StartOrder(); !slices.Equal(got, want) {
				t.Fatalf("StartOrder = %v, want accepted order %v", got, want)
			}
		})
	}
}

func TestConcurrentSubmitAndCheckpointDoNotDeadlock(t *testing.T) {
	engine, _, _, last := checkpointRuntime(t)
	const workers = 8
	errs := make(chan error, 2*workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			_, err := collect(t, engine, sampleRequest("session-1", fmt.Sprintf("turn-c%d", i)))
			errs <- err
		})
		wg.Go(func() {
			checkpoint := checkpointFixture(last)
			checkpoint.ResumeGeneration = uint64(i + 2)
			errs <- engine.SaveCheckpoint(context.Background(), checkpoint)
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent submit and checkpoint deadlocked")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent operation: %v", err)
		}
	}
}
