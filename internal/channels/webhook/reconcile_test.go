package webhook

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
	"github.com/anggasct/aura/internal/store/storetest"
)

const reconcilePassWait = 10 * time.Second

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type storedTurnEvent struct {
	id      string
	kind    string
	turnID  string
	payload string
}

type webhookTurnSeed struct {
	id          string
	events      []storedTurnEvent
	withSession bool
	expiresAt   time.Time
}

func seedWebhookTurn(t *testing.T, db *sql.DB, seed *webhookTurnSeed) *platformwebhook.Execution {
	t.Helper()
	now := time.Now().UTC()
	execution := &platformwebhook.Execution{
		ID: seed.id, KeyID: "primary", EventID: "evt-" + seed.id, Nonce: "nonce-" + seed.id, BodyDigest: "digest-" + seed.id,
		TurnID: "turn-" + seed.id, SessionID: "sess-" + seed.id, State: platformwebhook.StateAccepted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if !seed.expiresAt.IsZero() {
		execution.ExpiresAt = seed.expiresAt
	}
	executions, err := platformwebhook.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	if err := executions.InsertExecution(t.Context(), execution); err != nil {
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
			TurnID: cmp.Or(event.turnID, execution.TurnID), InvocationID: "inv-" + seed.id, Author: "webhook:primary",
			Kind: event.kind, SchemaVersion: 1, Payload: json.RawMessage(cmp.Or(event.payload, `{}`)), CreatedAt: now,
		}
		if err := events.Append(t.Context(), stored); err != nil {
			t.Fatalf("append %s: %v", stored.ID, err)
		}
	}
	return execution
}

func corruptStoredEvents(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `UPDATE runtime_event SET created_at = 'not-a-time' WHERE session_id = ?`, sessionID); err != nil {
		t.Fatalf("corrupt the events of %s: %v", sessionID, err)
	}
}

func replayOf(execution *platformwebhook.Execution) *platformwebhook.AcceptedEvent {
	event := testAcceptedEvent()
	event.KeyID, event.Nonce, event.BodyDigest, event.Envelope.EventID = execution.KeyID, execution.Nonce, execution.BodyDigest, execution.EventID
	return event
}

func readExecution(t *testing.T, db *sql.DB, id string) platformwebhook.Execution {
	t.Helper()
	executions, err := platformwebhook.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	execution, err := executions.Execution(t.Context(), id)
	if err != nil {
		t.Fatalf("Execution %s: %v", id, err)
	}
	return execution
}

type reconcileRecord struct {
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Settled int    `json:"settled"`
	Failed  int    `json:"failed"`
	Error   string `json:"error"`
}

func reconcileRecords(t *testing.T, logs *syncBuffer) []reconcileRecord {
	t.Helper()
	var records []reconcileRecord
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record reconcileRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func capturedLogger(logs *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(logs, nil))
}

func turnEmitting(kinds ...string) func(turnID string) []turnOutcome {
	return func(turnID string) []turnOutcome {
		outcomes := make([]turnOutcome, 0, len(kinds))
		for _, kind := range kinds {
			outcomes = append(outcomes, turnOutcome{event: turn.Event{ID: kind + "-" + turnID, Kind: kind}})
		}
		return outcomes
	}
}

func appendTurnCompleted(t *testing.T, db *sql.DB, execution *platformwebhook.Execution) {
	t.Helper()
	next := uint64(countRows(t, db, `SELECT COUNT(*) FROM runtime_event WHERE session_id = ?`, execution.SessionID)) + 1
	if err := turnstore.NewEventStore(db).Append(t.Context(), &turn.Event{
		ID: "done-" + execution.TurnID, SessionID: execution.SessionID, Sequence: next, TurnID: execution.TurnID,
		InvocationID: "inv-" + execution.TurnID, Author: "webhook:primary", Kind: turn.EventKindTurnCompleted,
		SchemaVersion: 1, Payload: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append the completed turn: %v", err)
	}
}

func TestWebhookDispatcher_ReplayOfAnAdmittedTurnSubmitsNothing(t *testing.T) {
	cases := []struct {
		name      string
		kinds     []string
		left      string
		completes bool
		want      string
	}{
		{name: "an execution left accepted", kinds: []string{turn.EventKindTurnAccepted},
			left: platformwebhook.StateAccepted, want: platformwebhook.StateAccepted},
		{name: "an execution left running whose turn ended", kinds: []string{turn.EventKindTurnAccepted, turn.EventKindModelStarted},
			left: platformwebhook.StateRunning, completes: true, want: platformwebhook.StateCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := storetest.OpenMigrated(t)
			first := &fakeTurnRuntime{events: turnEmitting(tc.kinds...)}
			dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: first})
			ref, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if err := dispatcher.Wait(t.Context()); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			left := readExecution(t, db, ref.ExecutionID)
			if left.State != tc.left {
				t.Fatalf("execution after the first process = %s, want %s", left.State, tc.left)
			}
			if tc.completes {
				appendTurnCompleted(t, db, &left)
			}
			second := &fakeTurnRuntime{events: acceptedTurnEvents}
			restarted, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: second})
			again, err := restarted.Dispatch(t.Context(), testAcceptedEvent())
			if err != nil || again != ref {
				t.Fatalf("replay after the restart = %+v, %v; want %+v", again, err, ref)
			}
			if got := readExecution(t, db, ref.ExecutionID).State; got != tc.want {
				t.Fatalf("execution when the replay returned = %s, want %s", got, tc.want)
			}
			if err := restarted.Wait(t.Context()); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if got := second.submitted(); got != 0 {
				t.Fatalf("turns submitted after the restart = %d, want 0", got)
			}
			if got := countRows(t, db, `SELECT COUNT(*) FROM session`); got != 1 {
				t.Fatalf("sessions = %d, want the one of the first dispatch", got)
			}
		})
	}
}

func TestWebhookReconcilePassSettlesUnfinishedExecutions(t *testing.T) {
	accepted := storedTurnEvent{id: "a", kind: turn.EventKindTurnAccepted}
	message := storedTurnEvent{id: "m", kind: turn.EventKindMessageCompleted}
	completed := storedTurnEvent{id: "d", kind: turn.EventKindTurnCompleted}

	t.Run("one pass settles the unexpired unfinished executions", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		executions, err := platformwebhook.NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_1ended", events: []storedTurnEvent{accepted, message, completed}})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_2open", events: []storedTurnEvent{accepted, {id: "s", kind: turn.EventKindModelStarted}}})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_3never"})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_4expired", events: []storedTurnEvent{accepted, completed}, expiresAt: time.Now().Add(-time.Minute)})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_5settled", events: []storedTurnEvent{accepted, completed}})
		if _, err := executions.MarkTerminal(t.Context(), "whex_5settled", platformwebhook.StateCompleted, "whex_5settled-d", "", time.Now()); err != nil {
			t.Fatalf("settle whex_5settled: %v", err)
		}
		settledBefore := readExecution(t, db, "whex_5settled")
		logs := &syncBuffer{}
		backend := &fakeTurnRuntime{events: acceptedTurnEvents}
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: backend, logger: capturedLogger(logs)})
		pass, err := dispatcher.ReconcilePass()
		if err != nil {
			t.Fatalf("ReconcilePass: %v", err)
		}
		runPassWithin(t, pass)
		for id, want := range map[string]string{
			"whex_1ended": platformwebhook.StateCompleted, "whex_2open": platformwebhook.StateRunning,
			"whex_3never": platformwebhook.StateAccepted, "whex_4expired": platformwebhook.StateAccepted,
		} {
			if got := readExecution(t, db, id).State; got != want {
				t.Errorf("%s = %s, want %s", id, got, want)
			}
		}
		if got := readExecution(t, db, "whex_1ended").ResultEventID; got != "whex_1ended-m" {
			t.Errorf("whex_1ended result = %q, want its completed message", got)
		}
		if got := readExecution(t, db, "whex_5settled"); got != settledBefore {
			t.Errorf("settled execution = %+v, want it unchanged %+v", got, settledBefore)
		}
		if got := backend.submitted(); got != 0 || countRows(t, db, `SELECT COUNT(*) FROM session WHERE id = 'sess-whex_3never'`) != 0 {
			t.Errorf("the pass submitted %d turns or created a session for the never admitted execution", got)
		}
		records := reconcileRecords(t, logs)
		if len(records) != 1 || records[0].Level != "INFO" || records[0].Msg != "webhook executions reconciled" || records[0].Settled != 1 {
			t.Fatalf("records = %+v, want one info line with settled 1", records)
		}
	})

	t.Run("a pass walks every batch behind the never admitted executions", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_a0"})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_a1"})
		settling := []string{"whex_b0", "whex_b1", "whex_b2"}
		for _, id := range settling {
			seedWebhookTurn(t, db, &webhookTurnSeed{id: id, events: []storedTurnEvent{accepted, completed}})
		}
		logs := &syncBuffer{}
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: &fakeTurnRuntime{events: acceptedTurnEvents}, logger: capturedLogger(logs)})
		pass, err := platformwebhook.NewReconcilePass(&platformwebhook.ReconcilePassOptions{
			Executions: dispatcher.executions,
			Reconciler: dispatcher.reconciler,
			Clock:      dispatcher.clock,
			Batch:      2,
			Logger:     dispatcher.logger,
		})
		if err != nil {
			t.Fatalf("NewReconcilePass: %v", err)
		}
		runPassWithin(t, pass)
		for _, id := range settling {
			if got := readExecution(t, db, id).State; got != platformwebhook.StateCompleted {
				t.Errorf("%s = %s, want completed", id, got)
			}
		}
		if records := reconcileRecords(t, logs); len(records) != 1 || records[0].Settled != 3 {
			t.Fatalf("records = %+v, want one line with settled 3", records)
		}
	})

	t.Run("a failing execution is counted and the pass goes on", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		broken := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_c0", events: []storedTurnEvent{accepted, completed}})
		corruptStoredEvents(t, db, broken.SessionID)
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_c1", events: []storedTurnEvent{accepted, completed}})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_c2", events: []storedTurnEvent{accepted, completed}})
		logs := &syncBuffer{}
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: &fakeTurnRuntime{events: acceptedTurnEvents}, logger: capturedLogger(logs)})
		pass, err := dispatcher.ReconcilePass()
		if err != nil {
			t.Fatalf("ReconcilePass: %v", err)
		}
		runPassWithin(t, pass)
		for id, want := range map[string]string{
			"whex_c0": platformwebhook.StateAccepted, "whex_c1": platformwebhook.StateCompleted, "whex_c2": platformwebhook.StateCompleted,
		} {
			if got := readExecution(t, db, id).State; got != want {
				t.Errorf("%s = %s, want %s", id, got, want)
			}
		}
		records := reconcileRecords(t, logs)
		if len(records) != 1 || records[0].Level != "WARN" || records[0].Msg != "webhook reconcile failed" ||
			records[0].Failed != 1 || records[0].Settled != 2 || !strings.Contains(records[0].Error, "whex_c0") {
			t.Fatalf("records = %+v, want one warning with failed 1, settled 2 and the error of whex_c0", records)
		}
	})
}

func runPassWithin(t *testing.T, pass *platformwebhook.ReconcilePass) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		pass.Pass(ctx)
		close(done)
	}()
	timeout := time.NewTimer(reconcilePassWait)
	defer timeout.Stop()
	select {
	case <-done:
	case <-timeout.C:
		cancel()
		<-done
		t.Fatal("the pass did not end")
	}
}

func requireOneReplayWarning(t *testing.T, logs *syncBuffer) {
	t.Helper()
	records := reconcileRecords(t, logs)
	if len(records) != 1 || records[0].Level != "WARN" || records[0].Msg != "webhook replay reconcile failed" {
		t.Fatalf("records = %+v, want one replay warning", records)
	}
}

func TestWebhookDispatcher_ReplayAnswersItsReferenceWhenTheReconcileFails(t *testing.T) {
	t.Run("a replay whose session read fails", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		execution := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_unreadable", events: []storedTurnEvent{{id: "a", kind: turn.EventKindTurnAccepted}}})
		corruptStoredEvents(t, db, execution.SessionID)
		logs := &syncBuffer{}
		backend := &fakeTurnRuntime{events: acceptedTurnEvents}
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: backend, logger: capturedLogger(logs)})
		ref, err := dispatcher.Dispatch(t.Context(), replayOf(execution))
		if err != nil || ref != (platformwebhook.ExecutionRef{ExecutionID: execution.ID, TurnID: execution.TurnID}) {
			t.Fatalf("replay = %+v, %v; want the original reference", ref, err)
		}
		if got := backend.submitted(); got != 0 {
			t.Fatalf("turns submitted = %d, want 0", got)
		}
		if got := readExecution(t, db, execution.ID).State; got != platformwebhook.StateAccepted {
			t.Fatalf("execution = %s, want it kept accepted", got)
		}
		requireOneReplayWarning(t, logs)
	})

	t.Run("a replay of a never admitted execution whose submission is refused", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		execution := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_refused"})
		logs := &syncBuffer{}
		backend := &fakeTurnRuntime{overload: true}
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: backend, logger: capturedLogger(logs)})
		ref, err := dispatcher.Dispatch(t.Context(), replayOf(execution))
		if err != nil || ref != (platformwebhook.ExecutionRef{ExecutionID: execution.ID, TurnID: execution.TurnID}) {
			t.Fatalf("replay = %+v, %v; want the original reference", ref, err)
		}
		if got := readExecution(t, db, execution.ID).State; got != platformwebhook.StateAccepted {
			t.Fatalf("execution = %s, want it kept accepted", got)
		}
		requireOneReplayWarning(t, logs)
	})

	t.Run("a live stream that breaks leaves the execution to the store", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		backend := &fakeTurnRuntime{events: func(turnID string) []turnOutcome {
			return []turnOutcome{
				{event: turn.Event{ID: "evt_accepted_" + turnID, Kind: turn.EventKindTurnAccepted}},
				{err: errors.New("stream broke")},
			}
		}}
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: backend})
		ref, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := dispatcher.Wait(t.Context()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got := readExecution(t, db, ref.ExecutionID); got.State != platformwebhook.StateAccepted || got.ErrorCode != "" {
			t.Fatalf("execution after a broken stream = %s %q, want it left accepted", got.State, got.ErrorCode)
		}
	})

	t.Run("a terminal event without an id leaves the execution to the store", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		execution := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_unstored", events: []storedTurnEvent{{id: "a", kind: turn.EventKindTurnAccepted}}})
		dispatcher, _ := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: &fakeTurnRuntime{events: acceptedTurnEvents}})
		yielded := false
		dispatcher.startDrain(t.Context(), execution.ID, func() (turn.Event, error, bool) {
			if yielded {
				return turn.Event{}, nil, false
			}
			yielded = true
			return turn.Event{Kind: turn.EventKindTurnFailed, TurnID: execution.TurnID, Payload: json.RawMessage(`{"code":"storage_unavailable"}`)}, nil, true
		}, func() {})
		if err := dispatcher.Wait(t.Context()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got := readExecution(t, db, execution.ID); got.State != platformwebhook.StateAccepted || got.ErrorCode != "" {
			t.Fatalf("execution after an unstored terminal event = %s %q, want it left accepted", got.State, got.ErrorCode)
		}
	})
}
