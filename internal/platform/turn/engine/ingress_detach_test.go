package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
)

const pollsAfterTheCancel = 10

type watchedRuntime struct {
	durable.Runtime

	mu       sync.Mutex
	statuses int
	cancels  int
	seen     chan struct{}
}

func watchRuntime(engine *Engine) *watchedRuntime {
	watched := &watchedRuntime{Runtime: engine.durableRuntime, seen: make(chan struct{}, 1)}
	engine.durableRuntime = watched
	return watched
}

func (w *watchedRuntime) note(count *int) {
	w.mu.Lock()
	*count++
	w.mu.Unlock()
	select {
	case w.seen <- struct{}{}:
	default:
	}
}

func (w *watchedRuntime) Status(ctx context.Context, run durable.RunRef) (durable.RunStatus, error) {
	w.note(&w.statuses)
	return w.Runtime.Status(ctx, run)
}

func (w *watchedRuntime) Cancel(ctx context.Context, run durable.RunRef) error {
	w.note(&w.cancels)
	return w.Runtime.Cancel(ctx, run)
}

func (w *watchedRuntime) counts() (statuses, cancels int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.statuses, w.cancels
}

func (w *watchedRuntime) awaitPollsOrCancel(t *testing.T, polls int) {
	t.Helper()
	from, _ := w.counts()
	deadline := time.NewTimer(hostRigTimeout)
	defer deadline.Stop()
	for {
		statuses, cancels := w.counts()
		if cancels > 0 || statuses-from >= polls {
			return
		}
		select {
		case <-w.seen:
		case <-deadline.C:
			t.Fatalf("the engine polled the run %d times after the cancel, want %d", statuses-from, polls)
		}
	}
}

type admissionRows struct {
	accepted, held, claims int
}

func admissionRowsOf(t *testing.T, db *sql.DB, sessionID string) admissionRows {
	t.Helper()
	var rows admissionRows
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM runtime_event WHERE session_id = ? AND kind = ?`, sessionID, turn.EventKindTurnAccepted).Scan(&rows.accepted); err != nil {
		t.Fatalf("count accepted events: %v", err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_session_turn WHERE session_id = ?`, sessionID).Scan(&rows.held); err != nil {
		t.Fatalf("count session turns: %v", err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM ingress_dedupe`).Scan(&rows.claims); err != nil {
		t.Fatalf("count dedupe claims: %v", err)
	}
	return rows
}

func stagedTurns(engine *Engine) int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return len(engine.turns) + len(engine.staged) + len(engine.admitting)
}

func TestAcceptedDurableTurnOutlivesItsCallersContext(t *testing.T) {
	gate := make(chan struct{})
	executor := turn.NewFakeExecutor([]turn.FakeStep{
		{Kind: turn.EventKindModelStarted, Payload: json.RawMessage(`{}`)},
		{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{}`), Block: gate},
	})
	rig := newDurableHostRig(t, Config{MaxActiveTurns: 2, MaxPendingTurns: 4}, executor)
	watched := watchRuntime(rig.engine)
	session := sampleSession("conv-detach")
	mustCreateSession(t, rig.db, session)
	rig.startHost()

	ended, endCaller := context.WithCancel(t.Context())
	endCaller()
	_, err := rig.engine.Accept(ended, sampleEnvelope("conv-detach", "detach-ended"))
	if code, ok := turn.CodeOf(err); !ok || code != turn.ErrorCodeStorageUnavailable {
		t.Fatalf("Accept under an ended context = %v, want %s", err, turn.ErrorCodeStorageUnavailable)
	}
	if got := admissionRowsOf(t, rig.db, session); got != (admissionRows{}) {
		t.Fatalf("rows after Accept under an ended context = %+v, want none", got)
	}
	if staged := stagedTurns(rig.engine); staged != 0 {
		t.Fatalf("turns held by the engine after Accept under an ended context = %d, want none", staged)
	}

	caller, cancelCaller := context.WithCancel(t.Context())
	defer cancelCaller()
	ref, err := rig.engine.Accept(caller, sampleEnvelope("conv-detach", "detach-live"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	eventually(t, "the executor starts the turn", func() bool { return executor.StartCount() == 1 })
	cancelCaller()
	watched.awaitPollsOrCancel(t, pollsAfterTheCancel)
	if _, cancels := watched.counts(); cancels != 0 {
		t.Fatalf("the runtime was asked to cancel the run %d times after the caller's context ended, want none", cancels)
	}
	if got := terminalCount(t, rig.db, ref.TurnID); got != 0 {
		t.Fatalf("terminal events after the caller's context ended = %d, want none", got)
	}
	close(gate)
	eventually(t, "the turn ends", func() bool { return terminalCount(t, rig.db, ref.TurnID) == 1 })
	if got := terminalRecordOf(t, rig.db, ref.TurnID); got.kind != turn.EventKindTurnCompleted {
		t.Fatalf("terminal = %+v, want %s", got, turn.EventKindTurnCompleted)
	}
}
