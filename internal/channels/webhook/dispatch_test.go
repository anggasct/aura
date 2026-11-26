package webhook

import (
	"context"
	"database/sql"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

type fakeTurnRuntime struct {
	mu        sync.Mutex
	calls     int
	overload  bool
	events    func(turnID string) []turnOutcome
	store     turnstore.EventStore
	sequences map[string]uint64
	parts     [][]runtimeingress.InputPart
	sessions  []string
}

type turnOutcome struct {
	event turn.Event
	err   error
}

func (f *fakeTurnRuntime) Run(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error] {
	f.mu.Lock()
	f.calls++
	f.parts = append(f.parts, req.Parts)
	f.sessions = append(f.sessions, req.SessionID)
	f.mu.Unlock()
	return func(yield func(turn.Event, error) bool) {
		if f.overload {
			yield(turn.Event{}, fault.New(turn.ErrorCodeRuntimeOverloaded, "pending turn queue is full"))
			return
		}
		outcomes := f.events(req.TurnID)
		for i := range outcomes {
			outcome := &outcomes[i]
			if outcome.err != nil {
				yield(turn.Event{}, outcome.err)
				return
			}
			event := outcome.event
			event.SessionID = req.SessionID
			event.TurnID = req.TurnID
			if event.InvocationID == "" {
				event.InvocationID = "inv_" + req.TurnID
			}
			if event.Author == "" {
				event.Author = req.PrincipalID
			}
			if event.SchemaVersion == 0 {
				event.SchemaVersion = 1
			}
			if len(event.Payload) == 0 {
				event.Payload = []byte(`{}`)
			}
			if event.CreatedAt.IsZero() {
				event.CreatedAt = time.Now().UTC()
			}
			f.mu.Lock()
			f.sequences[req.SessionID]++
			event.Sequence = f.sequences[req.SessionID]
			persist := f.store
			f.mu.Unlock()
			if persist != nil {
				if err := persist.Append(ctx, &event); err != nil {
					yield(turn.Event{}, err)
					return
				}
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (f *fakeTurnRuntime) submitted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTurnRuntime) submittedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sessions)
}

func acceptedTurnEvents(turnID string) []turnOutcome {
	return []turnOutcome{
		{event: turn.Event{ID: "evt_accepted_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnAccepted}},
		{event: turn.Event{ID: "evt_msg_" + turnID, TurnID: turnID, Kind: turn.EventKindMessageCompleted}},
		{event: turn.Event{ID: "evt_done_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnCompleted}},
	}
}

type dispatcherTestRig struct {
	db       *sql.DB
	backend  *fakeTurnRuntime
	sessions SessionStore
	logger   *slog.Logger
	handlers []EventHandler
}

func dispatcherTestSetup(t *testing.T, backend *fakeTurnRuntime) (*Dispatcher, platformwebhook.ExecutionStore) {
	t.Helper()
	return newTestDispatcher(t, &dispatcherTestRig{db: sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), principal.Schema(), storage.Schema(), platformwebhook.Schema()), backend: backend})
}

func newTestDispatcher(t *testing.T, rig *dispatcherTestRig) (*Dispatcher, platformwebhook.ExecutionStore) {
	t.Helper()
	executions, err := platformwebhook.NewSQLiteStore(rig.db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	rig.backend.store = turnstore.NewEventStore(rig.db)
	rig.backend.sequences = make(map[string]uint64)
	sessions := rig.sessions
	if sessions == nil {
		sessions = turnstore.NewSessionService(rig.db)
	}
	dispatcher, err := NewDispatcher(&DispatcherDeps{
		Sessions:   sessions,
		Events:     turnstore.NewTurnStore(rig.db),
		Executions: executions,
		Runtime:    rig.backend,
		Retention:  24 * time.Hour,
		Clock:      time.Now,
		Logger:     rig.logger,
		Handlers:   rig.handlers,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	t.Cleanup(func() {
		if err := dispatcher.Wait(context.Background()); err != nil {
			t.Errorf("dispatcher drains: %v", err)
		}
	})
	return dispatcher, executions
}

func testAcceptedEvent() *platformwebhook.AcceptedEvent {
	return &platformwebhook.AcceptedEvent{
		KeyID:      "primary",
		Nonce:      "nonce-abcdefghijklmnop",
		BodyDigest: "digest-1",
		Envelope: platformwebhook.Envelope{
			EventID:  "evt-1",
			Subject:  "hello",
			Payload:  []byte(`{"k":1}`),
			Metadata: map[string]string{"src": "ci"},
		},
	}
}

func waitForExecutionState(t *testing.T, executions platformwebhook.ExecutionStore, id, want string) platformwebhook.Execution {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		execution, err := executions.Execution(t.Context(), id)
		if err != nil {
			t.Fatalf("Execution: %v", err)
		}
		if execution.State == want {
			return execution
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution %s state = %q, want %q", id, execution.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows(%q): %v", query, err)
	}
	return n
}

func TestWebhookTurnRequestInvariants(t *testing.T) {
	event := testAcceptedEvent()
	req := TurnRequest("sess_1", "turn_1", event)
	if req.TurnID != "turn_1" || req.SessionID != "sess_1" {
		t.Fatalf("ids = %q/%q, want turn_1/sess_1", req.TurnID, req.SessionID)
	}
	if req.PrincipalID != "webhook:primary" || req.PrincipalClass != principal.ClassTrusted || req.Origin != turn.OriginWebhook {
		t.Fatalf("trust boundary = %q/%q/%q, want webhook:primary/trusted/webhook", req.PrincipalID, req.PrincipalClass, req.Origin)
	}
	if req.IdempotencyKey != "webhook:primary:evt-1" {
		t.Fatalf("idempotency key = %q, want webhook:primary:evt-1", req.IdempotencyKey)
	}
	if !req.Tainted() {
		t.Fatal("webhook turn request must be tainted")
	}
	if len(req.Parts) != 3 || req.Parts[0].Text != "hello" || req.Parts[1].Text != `{"k":1}` || req.Parts[2].Text != `{"src":"ci"}` {
		t.Fatalf("parts = %+v", req.Parts)
	}
}

func TestWebhookDispatcher_AcceptCompletesTurn(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := dispatcherTestSetup(t, backend)

	ref, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ref.ExecutionID == "" || ref.TurnID == "" {
		t.Fatalf("empty execution reference: %+v", ref)
	}

	execution := waitForExecutionState(t, executions, ref.ExecutionID, platformwebhook.StateCompleted)
	if execution.TurnID != ref.TurnID {
		t.Errorf("turn id = %q, want %q", execution.TurnID, ref.TurnID)
	}
	if got := backend.submittedSessions(); execution.SessionID == "" || !slices.Equal(got, []string{execution.SessionID}) {
		t.Errorf("execution session = %q, turn sessions = %v; want the session the turn ran in", execution.SessionID, got)
	}
	if execution.ResultEventID == "" {
		t.Error("expected a result event id on completion")
	}
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.parts) != 1 || len(backend.parts[0]) != 3 {
		t.Fatalf("turn parts = %v, want subject, payload, and metadata", backend.parts)
	}
	if backend.parts[0][0].Text != "hello" || backend.parts[0][1].Text != `{"k":1}` {
		t.Errorf("subject/payload parts = %q, %q", backend.parts[0][0].Text, backend.parts[0][1].Text)
	}
	if backend.parts[0][2].Text != `{"src":"ci"}` {
		t.Errorf("metadata part = %q, want %q", backend.parts[0][2].Text, `{"src":"ci"}`)
	}
}

func TestWebhookDispatcher_IdenticalReplayReturnsOriginal(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := dispatcherTestSetup(t, backend)

	first, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	second, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil {
		t.Fatalf("replay Dispatch: %v", err)
	}
	if first != second {
		t.Errorf("replay ref = %+v, want %+v", second, first)
	}
	waitForExecutionState(t, executions, first.ExecutionID, platformwebhook.StateCompleted)
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
}

func TestWebhookDispatcher_ChangedBodyConflicts(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := dispatcherTestSetup(t, backend)

	first, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	changed := testAcceptedEvent()
	changed.BodyDigest = "digest-2"
	if _, err := dispatcher.Dispatch(t.Context(), changed); err == nil {
		t.Fatal("expected replay conflict, got nil")
	} else if code, ok := fault.CodeOf[platformwebhook.ErrorCode](err); !ok || code != platformwebhook.ErrorCodeReplayConflict {
		t.Fatalf("conflict code = %v (ok=%v), want %s", code, ok, platformwebhook.ErrorCodeReplayConflict)
	}
	waitForExecutionState(t, executions, first.ExecutionID, platformwebhook.StateCompleted)
}

func TestWebhookDispatcher_OverloadLeavesNoRow(t *testing.T) {
	backend := &fakeTurnRuntime{overload: true}
	dispatcher, executions := dispatcherTestSetup(t, backend)

	if _, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent()); err == nil {
		t.Fatal("expected overload error, got nil")
	} else if code, ok := fault.CodeOf[platformwebhook.ErrorCode](err); !ok || code != platformwebhook.ErrorCodeRuntimeOverloaded {
		t.Fatalf("overload code = %v (ok=%v), want %s", code, ok, platformwebhook.ErrorCodeRuntimeOverloaded)
	}
	if _, found, err := executions.ExecutionByNonce(t.Context(), "primary", "nonce-abcdefghijklmnop"); err != nil || found {
		t.Errorf("overloaded request left an execution row: found=%v err=%v", found, err)
	}
}

func TestWebhookDispatcher_ConcurrentDuplicatesSubmitOnce(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := dispatcherTestSetup(t, backend)

	const callers = 8
	refs := make([]platformwebhook.ExecutionRef, callers)
	errs := make([]error, callers)
	var group sync.WaitGroup
	for i := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			ref, err := dispatcher.Dispatch(context.Background(), testAcceptedEvent())
			refs[i] = ref
			errs[i] = err
		}()
	}
	group.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if refs[i] != refs[0] || refs[i] == (platformwebhook.ExecutionRef{}) {
			t.Fatalf("caller %d ref = %+v, want %+v", i, refs[i], refs[0])
		}
	}
	waitForExecutionState(t, executions, refs[0].ExecutionID, platformwebhook.StateCompleted)
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
}

func TestWebhookDispatcher_OrphanedAcceptedReplayResubmits(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), principal.Schema(), storage.Schema(), platformwebhook.Schema())
	dispatcher, executions := newTestDispatcher(t, &dispatcherTestRig{db: db, backend: backend})
	ctx := t.Context()

	now := time.Now().UTC()
	orphan := &platformwebhook.Execution{
		ID:         "whex_orphan",
		KeyID:      "primary",
		EventID:    "evt-1",
		Nonce:      "nonce-abcdefghijklmnop",
		BodyDigest: "digest-1",
		TurnID:     "turn_orphan",
		SessionID:  "sess_orphan",
		State:      platformwebhook.StateAccepted,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
	if err := executions.InsertExecution(ctx, orphan); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}

	ref, err := dispatcher.Dispatch(ctx, testAcceptedEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ref.ExecutionID != "whex_orphan" || ref.TurnID != "turn_orphan" {
		t.Fatalf("replay ref = %+v, want the orphaned identity", ref)
	}
	execution := waitForExecutionState(t, executions, "whex_orphan", platformwebhook.StateCompleted)
	if got := backend.submittedSessions(); !slices.Equal(got, []string{"sess_orphan"}) {
		t.Errorf("submitted turn sessions = %v, want one turn in sess_orphan", got)
	}
	if execution.ResultEventID != "evt_msg_turn_orphan" {
		t.Errorf("result event = %q, want the message of the turn that ran", execution.ResultEventID)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM session`); got != 1 || countRows(t, db, `SELECT COUNT(*) FROM session WHERE id = 'sess_orphan'`) != 1 {
		t.Errorf("sessions = %d, want only sess_orphan", got)
	}
}

func TestWebhookDispatcher_FailedTurnRecordsCode(t *testing.T) {
	backend := &fakeTurnRuntime{events: func(turnID string) []turnOutcome {
		return []turnOutcome{
			{event: turn.Event{ID: "evt_a_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnAccepted}},
			{event: turn.Event{ID: "evt_f_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnFailed, Payload: []byte(`{"code":"model_capability_unsupported","detail":"no"}`)}},
		}
	}}
	dispatcher, executions := dispatcherTestSetup(t, backend)

	ref, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	execution := waitForExecutionState(t, executions, ref.ExecutionID, platformwebhook.StateFailed)
	if execution.ErrorCode != "model_capability_unsupported" {
		t.Errorf("error code = %q, want model_capability_unsupported", execution.ErrorCode)
	}
}

func TestWebhookDispatcherWaitTracksInFlightDrains(t *testing.T) {
	dispatcher, _ := dispatcherTestSetup(t, &fakeTurnRuntime{events: acceptedTurnEvents})
	release := make(chan struct{})
	stopped := make(chan struct{})
	dispatcher.startDrain(context.Background(), "exec-held", func() (turn.Event, error, bool) {
		<-release
		return turn.Event{}, nil, false
	}, func() { close(stopped) })
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := dispatcher.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with a drain in flight = %v, want the deadline", err)
	}
	close(release)
	if err := dispatcher.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after the drain finished = %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("drain finished without stopping its turn")
	}
}

func TestWebhookDispatcher_FinishedDrainIsNotResubmittedAsOrphan(t *testing.T) {
	backend := &fakeTurnRuntime{events: func(turnID string) []turnOutcome {
		return []turnOutcome{{event: turn.Event{ID: "evt_accepted_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnAccepted}}}
	}}
	dispatcher, executions := dispatcherTestSetup(t, backend)
	first, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := dispatcher.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	execution, found, err := executions.ExecutionByNonce(t.Context(), "primary", "nonce-abcdefghijklmnop")
	if err != nil || !found || execution.State != platformwebhook.StateAccepted {
		t.Fatalf("execution = %+v, %v, %v; want it left accepted by a turn that ended without a terminal event", execution, found, err)
	}
	again, err := dispatcher.Dispatch(t.Context(), testAcceptedEvent())
	if err != nil || again != first {
		t.Fatalf("duplicate Dispatch = %+v, %v; want %+v", again, err, first)
	}
	if err := dispatcher.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if backend.submitted() != 1 {
		t.Fatalf("submitted turns = %d, want 1: a drain this process already ran is not an orphan", backend.submitted())
	}
}
