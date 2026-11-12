package app

import (
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

	gatewaywebhook "github.com/anggasct/aura/internal/channels/webhook"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	testReconcileEventPage = 3
	reconcilePassWait      = 10 * time.Second
)

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

func seedWebhookTurn(t *testing.T, db *sql.DB, seed *webhookTurnSeed) *store.WebhookExecution {
	t.Helper()
	now := time.Now().UTC()
	execution := &store.WebhookExecution{
		ID: seed.id, KeyID: "primary", EventID: "evt-" + seed.id, Nonce: "nonce-" + seed.id, BodyDigest: "digest-" + seed.id,
		TurnID: "turn-" + seed.id, SessionID: "sess-" + seed.id, State: store.WebhookExecutionStateAccepted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if !seed.expiresAt.IsZero() {
		execution.ExpiresAt = seed.expiresAt
	}
	if err := store.NewWebhookExecutionStore(db).InsertExecution(t.Context(), execution); err != nil {
		t.Fatalf("InsertExecution %s: %v", seed.id, err)
	}
	if !seed.withSession && len(seed.events) == 0 {
		return execution
	}
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{
		ID: execution.SessionID, OwnerID: "webhook:primary", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session %s: %v", execution.SessionID, err)
	}
	events := store.NewEventStore(db)
	for i, event := range seed.events {
		stored := &store.RuntimeEvent{
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

func corruptStoredEvents(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `UPDATE runtime_event SET created_at = 'not-a-time' WHERE session_id = ?`, sessionID); err != nil {
		t.Fatalf("corrupt the events of %s: %v", sessionID, err)
	}
}

func replayOf(execution *store.WebhookExecution) *gatewaywebhook.AcceptedEvent {
	event := webhookTestEvent()
	event.KeyID, event.Nonce, event.BodyDigest, event.Envelope.EventID = execution.KeyID, execution.Nonce, execution.BodyDigest, execution.EventID
	return event
}

func readExecution(t *testing.T, db *sql.DB, id string) store.WebhookExecution {
	t.Helper()
	execution, err := store.NewWebhookExecutionStore(db).Execution(t.Context(), id)
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

func TestWebhookReconcileSettlesFromStoredEvents(t *testing.T) {
	accepted := storedTurnEvent{id: "a", kind: turn.EventKindTurnAccepted}
	started := storedTurnEvent{id: "s", kind: turn.EventKindModelStarted}
	message := storedTurnEvent{id: "m", kind: turn.EventKindMessageCompleted}
	completed := storedTurnEvent{id: "d", kind: turn.EventKindTurnCompleted}
	cases := []struct {
		name        string
		events      []storedTurnEvent
		withSession bool
		want        store.WebhookExecution
		result      reconcileResult
	}{
		{name: "only the accepted event", events: []storedTurnEvent{accepted},
			want: store.WebhookExecution{State: store.WebhookExecutionStateAccepted}, result: reconcileResult{admitted: true}},
		{name: "one more event", events: []storedTurnEvent{accepted, started},
			want: store.WebhookExecution{State: store.WebhookExecutionStateRunning}, result: reconcileResult{admitted: true}},
		{name: "a completed message before the completed turn", events: []storedTurnEvent{accepted, message, completed},
			want: store.WebhookExecution{State: store.WebhookExecutionStateCompleted, ResultEventID: "m"}, result: reconcileResult{admitted: true, settled: true}},
		{name: "a completed turn without a message", events: []storedTurnEvent{accepted, completed},
			want: store.WebhookExecution{State: store.WebhookExecutionStateCompleted, ResultEventID: "d"}, result: reconcileResult{admitted: true, settled: true}},
		{name: "a failed turn with a code", events: []storedTurnEvent{accepted, {id: "f", kind: turn.EventKindTurnFailed, payload: `{"code":"model_capability_unsupported"}`}},
			want: store.WebhookExecution{State: store.WebhookExecutionStateFailed, ResultEventID: "f", ErrorCode: "model_capability_unsupported"}, result: reconcileResult{admitted: true, settled: true}},
		{name: "a failed turn without a code", events: []storedTurnEvent{accepted, {id: "f", kind: turn.EventKindTurnFailed}},
			want: store.WebhookExecution{State: store.WebhookExecutionStateFailed, ResultEventID: "f", ErrorCode: string(turn.ErrorCodeRuntimeInternal)}, result: reconcileResult{admitted: true, settled: true}},
		{name: "a cancelled turn", events: []storedTurnEvent{accepted, {id: "c", kind: turn.EventKindTurnCancelled}},
			want: store.WebhookExecution{State: store.WebhookExecutionStateCancelled, ErrorCode: string(turn.ErrorCodeTurnCancelled)}, result: reconcileResult{admitted: true, settled: true}},
		{name: "no event in an existing session", withSession: true,
			want: store.WebhookExecution{State: store.WebhookExecutionStateAccepted}},
		{name: "no event and no session",
			want: store.WebhookExecution{State: store.WebhookExecutionStateAccepted}},
		{name: "a turn longer than one page ends", events: []storedTurnEvent{accepted, started, {id: "s2", kind: turn.EventKindModelStarted}, {id: "s3", kind: turn.EventKindModelStarted}, completed},
			want: store.WebhookExecution{State: store.WebhookExecutionStateCompleted, ResultEventID: "d"}, result: reconcileResult{admitted: true, settled: true}},
		{name: "only an event of another turn", events: []storedTurnEvent{{id: "w", kind: turn.EventKindMessageCompleted, turnID: "turn-workflow"}},
			want: store.WebhookExecution{State: store.WebhookExecutionStateAccepted}},
		{name: "an event of the turn after its terminal one", events: []storedTurnEvent{accepted, completed, {id: "x", kind: turn.EventKindModelStarted}},
			want: store.WebhookExecution{State: store.WebhookExecutionStateCompleted, ResultEventID: "d"}, result: reconcileResult{admitted: true, settled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := storetest.OpenMigrated(t)
			execution := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_case", events: tc.events, withSession: tc.withSession})
			reconciler := &webhookReconciler{
				sessions: store.NewSessionService(db), executions: store.NewWebhookExecutionStore(db), clock: time.Now, page: testReconcileEventPage,
			}
			result, err := reconciler.reconcile(t.Context(), execution)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if result != tc.result {
				t.Fatalf("reconcile = %+v, want %+v", result, tc.result)
			}
			got := readExecution(t, db, execution.ID)
			want := tc.want
			if want.ResultEventID != "" {
				want.ResultEventID = execution.ID + "-" + want.ResultEventID
			}
			if got.State != want.State || got.ResultEventID != want.ResultEventID || got.ErrorCode != want.ErrorCode {
				t.Fatalf("execution = %s %q %q, want %s %q %q", got.State, got.ResultEventID, got.ErrorCode, want.State, want.ResultEventID, want.ErrorCode)
			}
			again, err := reconciler.reconcile(t.Context(), &got)
			if err != nil || again.settled {
				t.Fatalf("second reconcile = %+v, %v; want no new settlement", again, err)
			}
			if after := readExecution(t, db, execution.ID); after != got {
				t.Fatalf("second reconcile changed the row: %+v, want %+v", after, got)
			}
		})
	}
}

func turnEmitting(kinds ...string) func(turnID string) []turnOutcome {
	return func(turnID string) []turnOutcome {
		outcomes := make([]turnOutcome, 0, len(kinds))
		for _, kind := range kinds {
			outcomes = append(outcomes, turnOutcome{event: store.RuntimeEvent{ID: kind + "-" + turnID, Kind: kind}})
		}
		return outcomes
	}
}

func appendTurnCompleted(t *testing.T, db *sql.DB, execution *store.WebhookExecution) {
	t.Helper()
	next := uint64(countRows(t, db, `SELECT COUNT(*) FROM runtime_event WHERE session_id = ?`, execution.SessionID)) + 1
	if err := store.NewEventStore(db).Append(t.Context(), &store.RuntimeEvent{
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
			left: store.WebhookExecutionStateAccepted, want: store.WebhookExecutionStateAccepted},
		{name: "an execution left running whose turn ended", kinds: []string{turn.EventKindTurnAccepted, turn.EventKindModelStarted},
			left: store.WebhookExecutionStateRunning, completes: true, want: store.WebhookExecutionStateCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := storetest.OpenMigrated(t)
			first := &fakeTurnRuntime{events: turnEmitting(tc.kinds...)}
			dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: first})
			ref, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
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
			restarted, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: second})
			again, err := restarted.Dispatch(t.Context(), webhookTestEvent())
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
		executions := store.NewWebhookExecutionStore(db)
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_1ended", events: []storedTurnEvent{accepted, message, completed}})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_2open", events: []storedTurnEvent{accepted, {id: "s", kind: turn.EventKindModelStarted}}})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_3never"})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_4expired", events: []storedTurnEvent{accepted, completed}, expiresAt: time.Now().Add(-time.Minute)})
		seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_5settled", events: []storedTurnEvent{accepted, completed}})
		if _, err := executions.MarkTerminal(t.Context(), "whex_5settled", store.WebhookExecutionStateCompleted, "whex_5settled-d", "", time.Now()); err != nil {
			t.Fatalf("settle whex_5settled: %v", err)
		}
		settledBefore := readExecution(t, db, "whex_5settled")
		logs := &syncBuffer{}
		backend := &fakeTurnRuntime{events: acceptedTurnEvents}
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: backend, logger: capturedLogger(logs)})
		runPassWithin(t, dispatcher.reconcilePass())
		for id, want := range map[string]string{
			"whex_1ended": store.WebhookExecutionStateCompleted, "whex_2open": store.WebhookExecutionStateRunning,
			"whex_3never": store.WebhookExecutionStateAccepted, "whex_4expired": store.WebhookExecutionStateAccepted,
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
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: &fakeTurnRuntime{events: acceptedTurnEvents}, logger: capturedLogger(logs)})
		pass := dispatcher.reconcilePass()
		pass.batch = 2
		runPassWithin(t, pass)
		for _, id := range settling {
			if got := readExecution(t, db, id).State; got != store.WebhookExecutionStateCompleted {
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
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: &fakeTurnRuntime{events: acceptedTurnEvents}, logger: capturedLogger(logs)})
		runPassWithin(t, dispatcher.reconcilePass())
		for id, want := range map[string]string{
			"whex_c0": store.WebhookExecutionStateAccepted, "whex_c1": store.WebhookExecutionStateCompleted, "whex_c2": store.WebhookExecutionStateCompleted,
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

func runPassWithin(t *testing.T, pass *webhookReconcilePass) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		pass.pass(ctx)
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

type scriptedUnfinished struct {
	mu       sync.Mutex
	failures []error
	lists    int
}

func (s *scriptedUnfinished) ListUnfinished(context.Context, *store.UnfinishedExecutionQuery) ([]store.WebhookExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.lists <= len(s.failures) && s.failures[s.lists-1] != nil {
		return nil, s.failures[s.lists-1]
	}
	return []store.WebhookExecution{{ID: "whex_listed"}}, nil
}

func (s *scriptedUnfinished) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

type reconcileScript struct {
	failures []error
	settles  bool
}

func newScriptedReconcilePass(script reconcileScript) (*webhookReconcilePass, *scriptedUnfinished, *syncBuffer) {
	lister := &scriptedUnfinished{failures: script.failures}
	logs := &syncBuffer{}
	return &webhookReconcilePass{
		executions: lister,
		reconcile: func(context.Context, *store.WebhookExecution) (reconcileResult, error) {
			return reconcileResult{admitted: true, settled: script.settles}, nil
		},
		clock:    time.Now,
		batch:    webhookReconcileBatch,
		interval: webhookReconcileInterval,
		logger:   capturedLogger(logs),
	}, lister, logs
}

func runReconcilePasses(t *testing.T, pass *webhookReconcilePass, intervals int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		pass.run(ctx)
		close(done)
	}()
	time.Sleep(time.Duration(intervals)*pass.interval + pass.interval/2)
	synctest.Wait()
	cancel()
	<-done
}

func countRecords(records []reconcileRecord, level, msg string) int {
	count := 0
	for _, record := range records {
		if record.Level == level && record.Msg == msg {
			count++
		}
	}
	return count
}

func TestWebhookReconcilePassRepeats(t *testing.T) {
	t.Run("one pass at the start and one per interval, silent when nothing settled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pass, lister, logs := newScriptedReconcilePass(reconcileScript{})
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
			pass, lister, logs := newScriptedReconcilePass(reconcileScript{failures: []error{locked, locked}, settles: true})
			runReconcilePasses(t, pass, 2)
			if got := lister.count(); got != 3 {
				t.Fatalf("passes after two intervals = %d, want 3", got)
			}
			records := reconcileRecords(t, logs)
			if got := countRecords(records, "WARN", "webhook reconcile failed"); got != 2 {
				t.Fatalf("records = %+v, want one warning for each failing pass", records)
			}
			if got := countRecords(records, "INFO", "webhook executions reconciled"); got != 1 || len(records) != 3 {
				t.Fatalf("records = %+v, want one info line for the pass that settled", records)
			}
		})
	})

	t.Run("nothing runs or is logged under an ended context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pass, lister, logs := newScriptedReconcilePass(reconcileScript{failures: []error{errors.New("database is locked")}})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			pass.run(ctx)
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
			pass, _, logs := newScriptedReconcilePass(reconcileScript{})
			ctx, cancel := context.WithCancel(t.Context())
			pass.reconcile = func(ctx context.Context, _ *store.WebhookExecution) (reconcileResult, error) {
				cancel()
				return reconcileResult{}, ctx.Err()
			}
			pass.pass(ctx)
			if got := logs.String(); got != "" {
				t.Fatalf("logs of a pass whose context ended = %q, want none", got)
			}
		})
	})
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
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: backend, logger: capturedLogger(logs)})
		ref, err := dispatcher.Dispatch(t.Context(), replayOf(execution))
		if err != nil || ref != (gatewaywebhook.ExecutionRef{ExecutionID: execution.ID, TurnID: execution.TurnID}) {
			t.Fatalf("replay = %+v, %v; want the original reference", ref, err)
		}
		if got := backend.submitted(); got != 0 {
			t.Fatalf("turns submitted = %d, want 0", got)
		}
		if got := readExecution(t, db, execution.ID).State; got != store.WebhookExecutionStateAccepted {
			t.Fatalf("execution = %s, want it kept accepted", got)
		}
		requireOneReplayWarning(t, logs)
	})

	t.Run("a replay of a never admitted execution whose submission is refused", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		execution := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_refused"})
		logs := &syncBuffer{}
		backend := &fakeTurnRuntime{overload: true}
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: backend, logger: capturedLogger(logs)})
		ref, err := dispatcher.Dispatch(t.Context(), replayOf(execution))
		if err != nil || ref != (gatewaywebhook.ExecutionRef{ExecutionID: execution.ID, TurnID: execution.TurnID}) {
			t.Fatalf("replay = %+v, %v; want the original reference", ref, err)
		}
		if got := readExecution(t, db, execution.ID).State; got != store.WebhookExecutionStateAccepted {
			t.Fatalf("execution = %s, want it kept accepted", got)
		}
		requireOneReplayWarning(t, logs)
	})

	t.Run("a live stream that breaks leaves the execution to the store", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		backend := &fakeTurnRuntime{events: func(turnID string) []turnOutcome {
			return []turnOutcome{
				{event: store.RuntimeEvent{ID: "evt_accepted_" + turnID, Kind: turn.EventKindTurnAccepted}},
				{err: errors.New("stream broke")},
			}
		}}
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: backend})
		ref, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if err := dispatcher.Wait(t.Context()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got := readExecution(t, db, ref.ExecutionID); got.State != store.WebhookExecutionStateAccepted || got.ErrorCode != "" {
			t.Fatalf("execution after a broken stream = %s %q, want it left accepted", got.State, got.ErrorCode)
		}
	})

	t.Run("a terminal event without an id leaves the execution to the store", func(t *testing.T) {
		db := storetest.OpenMigrated(t)
		execution := seedWebhookTurn(t, db, &webhookTurnSeed{id: "whex_unstored", events: []storedTurnEvent{{id: "a", kind: turn.EventKindTurnAccepted}}})
		dispatcher, _ := newWebhookTestDispatcher(t, &webhookTestRig{db: db, backend: &fakeTurnRuntime{events: acceptedTurnEvents}})
		yielded := false
		dispatcher.startDrain(t.Context(), execution.ID, func() (store.RuntimeEvent, error, bool) {
			if yielded {
				return store.RuntimeEvent{}, nil, false
			}
			yielded = true
			return store.RuntimeEvent{Kind: turn.EventKindTurnFailed, TurnID: execution.TurnID, Payload: json.RawMessage(`{"code":"storage_unavailable"}`)}, nil, true
		}, func() {})
		if err := dispatcher.Wait(t.Context()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got := readExecution(t, db, execution.ID); got.State != store.WebhookExecutionStateAccepted || got.ErrorCode != "" {
			t.Fatalf("execution after an unstored terminal event = %s %q, want it left accepted", got.State, got.ErrorCode)
		}
	})
}
