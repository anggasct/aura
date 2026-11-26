package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const testReconcileEventPage = 3

type reconcileSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *reconcileSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *reconcileSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type reconcileStoredTurnEvent struct {
	id      string
	kind    string
	turnID  string
	payload string
}

type reconcileTurnSeed struct {
	id          string
	events      []reconcileStoredTurnEvent
	withSession bool
	expiresAt   time.Time
}

func seedReconcileTurn(t *testing.T, db *sql.DB, seed *reconcileTurnSeed) *Execution {
	t.Helper()
	now := time.Now().UTC()
	execution := &Execution{
		ID: seed.id, KeyID: "primary", EventID: "evt-" + seed.id, Nonce: "nonce-" + seed.id, BodyDigest: "digest-" + seed.id,
		TurnID: "turn-" + seed.id, SessionID: "sess-" + seed.id, State: StateAccepted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if !seed.expiresAt.IsZero() {
		execution.ExpiresAt = seed.expiresAt
	}
	if err := mustNewSQLiteStore(t, db).InsertExecution(t.Context(), execution); err != nil {
		t.Fatalf("InsertExecution %s: %v", seed.id, err)
	}
	if !seed.withSession && len(seed.events) == 0 {
		return execution
	}
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{
		ID: execution.SessionID, OwnerID: "webhook:primary", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session %s: %v", execution.SessionID, err)
	}
	events := turnstore.NewEventStore(db)
	for i, event := range seed.events {
		stored := &turn.Event{
			ID: seed.id + "-" + event.id, SessionID: execution.SessionID, Sequence: uint64(i + 1),
			TurnID: firstNonEmpty(event.turnID, execution.TurnID), InvocationID: "inv-" + seed.id, Author: "webhook:primary",
			Kind: event.kind, SchemaVersion: 1, Payload: json.RawMessage(firstNonEmpty(event.payload, `{}`)), CreatedAt: now,
		}
		if err := events.Append(t.Context(), stored); err != nil {
			t.Fatalf("append %s: %v", stored.ID, err)
		}
	}
	return execution
}

func readExecutionForTest(t *testing.T, db *sql.DB, id string) Execution {
	t.Helper()
	execution, err := mustNewSQLiteStore(t, db).Execution(t.Context(), id)
	if err != nil {
		t.Fatalf("Execution %s: %v", id, err)
	}
	return execution
}

type reconcileLogRecord struct {
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Settled int    `json:"settled"`
	Failed  int    `json:"failed"`
	Error   string `json:"error"`
}

func parseReconcileRecords(t *testing.T, logs *reconcileSyncBuffer) []reconcileLogRecord {
	t.Helper()
	var records []reconcileLogRecord
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record reconcileLogRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func countReconcileRecords(records []reconcileLogRecord, level, msg string) int {
	count := 0
	for _, record := range records {
		if record.Level == level && record.Msg == msg {
			count++
		}
	}
	return count
}

func TestWebhookReconcileSettlesFromStoredEvents(t *testing.T) {
	t.Run("nil event is ignored by derivation", func(t *testing.T) {
		var derivation ExecutionDerivation
		outcome, terminal := derivation.Next(nil)
		if outcome != (ExecutionOutcome{}) || terminal {
			t.Fatalf("Next(nil) = %+v, %v; want zero outcome and false", outcome, terminal)
		}
	})

	accepted := reconcileStoredTurnEvent{id: "a", kind: turn.EventKindTurnAccepted}
	started := reconcileStoredTurnEvent{id: "s", kind: turn.EventKindModelStarted}
	message := reconcileStoredTurnEvent{id: "m", kind: turn.EventKindMessageCompleted}
	completed := reconcileStoredTurnEvent{id: "d", kind: turn.EventKindTurnCompleted}
	cases := []struct {
		name        string
		events      []reconcileStoredTurnEvent
		withSession bool
		want        Execution
		result      ReconcileResult
	}{
		{name: "only the accepted event", events: []reconcileStoredTurnEvent{accepted},
			want: Execution{State: StateAccepted}, result: ReconcileResult{Admitted: true}},
		{name: "one more event", events: []reconcileStoredTurnEvent{accepted, started},
			want: Execution{State: StateRunning}, result: ReconcileResult{Admitted: true}},
		{name: "a completed message before the completed turn", events: []reconcileStoredTurnEvent{accepted, message, completed},
			want: Execution{State: StateCompleted, ResultEventID: "m"}, result: ReconcileResult{Admitted: true, Settled: true}},
		{name: "a completed turn without a message", events: []reconcileStoredTurnEvent{accepted, completed},
			want: Execution{State: StateCompleted, ResultEventID: "d"}, result: ReconcileResult{Admitted: true, Settled: true}},
		{name: "a failed turn with a code", events: []reconcileStoredTurnEvent{accepted, {id: "f", kind: turn.EventKindTurnFailed, payload: `{"code":"model_capability_unsupported"}`}},
			want: Execution{State: StateFailed, ResultEventID: "f", ErrorCode: "model_capability_unsupported"}, result: ReconcileResult{Admitted: true, Settled: true}},
		{name: "a failed turn without a code", events: []reconcileStoredTurnEvent{accepted, {id: "f", kind: turn.EventKindTurnFailed}},
			want: Execution{State: StateFailed, ResultEventID: "f", ErrorCode: string(turn.ErrorCodeRuntimeInternal)}, result: ReconcileResult{Admitted: true, Settled: true}},
		{name: "a cancelled turn", events: []reconcileStoredTurnEvent{accepted, {id: "c", kind: turn.EventKindTurnCancelled}},
			want: Execution{State: StateCancelled, ErrorCode: string(turn.ErrorCodeTurnCancelled)}, result: ReconcileResult{Admitted: true, Settled: true}},
		{name: "no event in an existing session", withSession: true,
			want: Execution{State: StateAccepted}},
		{name: "no event and no session",
			want: Execution{State: StateAccepted}},
		{name: "a turn longer than one page ends", events: []reconcileStoredTurnEvent{accepted, started, {id: "s2", kind: turn.EventKindModelStarted}, {id: "s3", kind: turn.EventKindModelStarted}, completed},
			want: Execution{State: StateCompleted, ResultEventID: "d"}, result: ReconcileResult{Admitted: true, Settled: true}},
		{name: "only an event of another turn", events: []reconcileStoredTurnEvent{{id: "w", kind: turn.EventKindMessageCompleted, turnID: "turn-workflow"}},
			want: Execution{State: StateAccepted}},
		{name: "an event of the turn after its terminal one", events: []reconcileStoredTurnEvent{accepted, completed, {id: "x", kind: turn.EventKindModelStarted}},
			want: Execution{State: StateCompleted, ResultEventID: "d"}, result: ReconcileResult{Admitted: true, Settled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
			execution := seedReconcileTurn(t, db, &reconcileTurnSeed{id: "whex_case", events: tc.events, withSession: tc.withSession})
			reconciler, err := NewReconciler(&ReconcilerDeps{
				Events:     turnstore.NewTurnStore(db),
				Executions: mustNewSQLiteStore(t, db),
				Clock:      time.Now,
				EventPage:  testReconcileEventPage,
			})
			if err != nil {
				t.Fatalf("NewReconciler: %v", err)
			}
			result, err := reconciler.Reconcile(t.Context(), execution)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result != tc.result {
				t.Fatalf("Reconcile = %+v, want %+v", result, tc.result)
			}
			got := readExecutionForTest(t, db, execution.ID)
			want := tc.want
			if want.ResultEventID != "" {
				want.ResultEventID = execution.ID + "-" + want.ResultEventID
			}
			if got.State != want.State || got.ResultEventID != want.ResultEventID || got.ErrorCode != want.ErrorCode {
				t.Fatalf("execution = %s %q %q, want %s %q %q", got.State, got.ResultEventID, got.ErrorCode, want.State, want.ResultEventID, want.ErrorCode)
			}
			again, err := reconciler.Reconcile(t.Context(), &got)
			if err != nil || again.Settled {
				t.Fatalf("second Reconcile = %+v, %v; want no new settlement", again, err)
			}
			if after := readExecutionForTest(t, db, execution.ID); after != got {
				t.Fatalf("second Reconcile changed the row: %+v, want %+v", after, got)
			}
		})
	}
}

type scriptedUnfinished struct {
	mu       sync.Mutex
	failures []error
	lists    int
}

func (s *scriptedUnfinished) ListUnfinished(context.Context, *UnfinishedQuery) ([]Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.lists <= len(s.failures) && s.failures[s.lists-1] != nil {
		return nil, s.failures[s.lists-1]
	}
	return []Execution{{ID: "whex_listed"}}, nil
}

func (s *scriptedUnfinished) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

type reconcileScript struct {
	failures  []error
	settles   bool
	reconcile func(ctx context.Context, execution *Execution) (ReconcileResult, error)
}

func newScriptedReconcilePass(t *testing.T, script reconcileScript) (*ReconcilePass, *scriptedUnfinished, *reconcileSyncBuffer) {
	t.Helper()
	lister := &scriptedUnfinished{failures: script.failures}
	logs := &reconcileSyncBuffer{}
	reconcileFn := script.reconcile
	if reconcileFn == nil {
		reconcileFn = func(context.Context, *Execution) (ReconcileResult, error) {
			return ReconcileResult{Admitted: true, Settled: script.settles}, nil
		}
	}
	pass, err := NewReconcilePass(&ReconcilePassOptions{
		Executions: lister,
		Reconcile:  reconcileFn,
		Clock:      time.Now,
		Batch:      DefaultReconcileBatch,
		Interval:   DefaultReconcileInterval,
		Logger:     slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewReconcilePass: %v", err)
	}
	return pass, lister, logs
}

func runReconcilePasses(t *testing.T, pass *ReconcilePass, intervals int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		pass.Run(ctx)
		close(done)
	}()
	time.Sleep(time.Duration(intervals)*pass.interval + pass.interval/2)
	synctest.Wait()
	cancel()
	<-done
}

func TestWebhookReconcilePassRepeats(t *testing.T) {
	t.Run("one pass at the start and one per interval, silent when nothing settled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pass, lister, logs := newScriptedReconcilePass(t, reconcileScript{})
			runReconcilePasses(t, pass, 3)
			if got := lister.count(); got != 4 {
				t.Fatalf("passes after three intervals = %d, want 4", got)
			}
			if got := logs.String(); got != "" {
				t.Fatalf("logs = %q, want none when nothing settled", got)
			}
		})
	})

	t.Run("a failing list is one warning per pass and the next pass runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			locked := errors.New("database is locked")
			pass, lister, logs := newScriptedReconcilePass(t, reconcileScript{failures: []error{locked, locked}, settles: true})
			runReconcilePasses(t, pass, 2)
			if got := lister.count(); got != 3 {
				t.Fatalf("passes after two intervals = %d, want 3", got)
			}
			records := parseReconcileRecords(t, logs)
			if got := countReconcileRecords(records, "WARN", "webhook reconcile failed"); got != 2 {
				t.Fatalf("records = %+v, want one warning for each failing pass", records)
			}
			if got := countReconcileRecords(records, "INFO", "webhook executions reconciled"); got != 1 || len(records) != 3 {
				t.Fatalf("records = %+v, want one info line for the pass that settled", records)
			}
		})
	})

	t.Run("nothing runs or is logged under an ended context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pass, lister, logs := newScriptedReconcilePass(t, reconcileScript{failures: []error{errors.New("database is locked")}})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			pass.Run(ctx)
			if got := lister.count(); got != 0 {
				t.Fatalf("passes under an ended context = %d, want 0", got)
			}
			if got := logs.String(); got != "" {
				t.Fatalf("logs under an ended context = %q, want none", got)
			}
		})
	})

	t.Run("a pass whose context ends while it runs logs nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			pass, _, logs := newScriptedReconcilePass(t, reconcileScript{
				reconcile: func(ctx context.Context, _ *Execution) (ReconcileResult, error) {
					cancel()
					return ReconcileResult{}, ctx.Err()
				},
			})
			pass.Pass(ctx)
			if got := logs.String(); got != "" {
				t.Fatalf("logs of a pass whose context ended = %q, want none", got)
			}
		})
	})
}
