package cli

import (
	"context"
	"database/sql"
	"iter"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
	gatewaywebhook "github.com/anggasct/aura/internal/gateway/webhook"
	"github.com/anggasct/aura/internal/integration/github"
	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/workflow"
)

type fakeTurnRuntime struct {
	mu        sync.Mutex
	calls     int
	overload  bool
	events    func(turnID string) []turnOutcome
	store     store.EventStore
	sequences map[string]uint64
	parts     [][]runtimeingress.InputPart
}

type turnOutcome struct {
	event store.RuntimeEvent
	err   error
}

func (f *fakeTurnRuntime) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	f.mu.Lock()
	f.calls++
	f.parts = append(f.parts, req.Parts)
	f.mu.Unlock()
	return func(yield func(store.RuntimeEvent, error) bool) {
		if f.overload {
			yield(store.RuntimeEvent{}, &runtime.Error{Code: runtime.ErrorCodeRuntimeOverloaded, Detail: "pending turn queue is full"})
			return
		}
		outcomes := f.events(req.TurnID)
		for i := range outcomes {
			outcome := &outcomes[i]
			if outcome.err != nil {
				yield(store.RuntimeEvent{}, outcome.err)
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
					yield(store.RuntimeEvent{}, err)
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

func acceptedTurnEvents(turnID string) []turnOutcome {
	return []turnOutcome{
		{event: store.RuntimeEvent{ID: "evt_accepted_" + turnID, TurnID: turnID, Kind: runtime.EventKindTurnAccepted}},
		{event: store.RuntimeEvent{ID: "evt_msg_" + turnID, TurnID: turnID, Kind: runtime.EventKindMessageCompleted}},
		{event: store.RuntimeEvent{ID: "evt_done_" + turnID, TurnID: turnID, Kind: runtime.EventKindTurnCompleted}},
	}
}

func webhookTestSetup(t *testing.T, backend *fakeTurnRuntime) (*webhookDispatcher, store.WebhookExecutionStore) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, t.TempDir()+"/aura.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	executions := store.NewWebhookExecutionStore(db)
	backend.store = store.NewEventStore(db)
	backend.sequences = make(map[string]uint64)
	dispatcher, err := newWebhookDispatcher(store.NewSessionService(db), executions, backend, 24*time.Hour, time.Now, nil)
	if err != nil {
		t.Fatalf("newWebhookDispatcher: %v", err)
	}
	return dispatcher, executions
}

func webhookTestEvent() *gatewaywebhook.AcceptedEvent {
	return &gatewaywebhook.AcceptedEvent{
		KeyID:      "primary",
		Nonce:      "nonce-abcdefghijklmnop",
		BodyDigest: "digest-1",
		Envelope: gatewaywebhook.Envelope{
			EventID:  "evt-1",
			Subject:  "hello",
			Payload:  []byte(`{"k":1}`),
			Metadata: map[string]string{"src": "ci"},
		},
	}
}

func waitForExecutionState(t *testing.T, executions store.WebhookExecutionStore, id, want string) store.WebhookExecution {
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

func TestWebhookDispatcher_AcceptCompletesTurn(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := webhookTestSetup(t, backend)

	ref, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ref.ExecutionID == "" || ref.TurnID == "" {
		t.Fatalf("empty execution reference: %+v", ref)
	}

	execution := waitForExecutionState(t, executions, ref.ExecutionID, store.WebhookExecutionStateCompleted)
	if execution.TurnID != ref.TurnID {
		t.Errorf("turn id = %q, want %q", execution.TurnID, ref.TurnID)
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
	dispatcher, executions := webhookTestSetup(t, backend)

	first, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	second, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
	if err != nil {
		t.Fatalf("replay Dispatch: %v", err)
	}
	if first != second {
		t.Errorf("replay ref = %+v, want %+v", second, first)
	}
	waitForExecutionState(t, executions, first.ExecutionID, store.WebhookExecutionStateCompleted)
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
}

func TestWebhookDispatcher_ChangedBodyConflicts(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := webhookTestSetup(t, backend)

	first, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	changed := webhookTestEvent()
	changed.BodyDigest = "digest-2"
	if _, err := dispatcher.Dispatch(t.Context(), changed); err == nil {
		t.Fatal("expected replay conflict, got nil")
	} else if code, ok := gatewaywebhook.CodeOf(err); !ok || code != gatewaywebhook.ErrorCodeReplayConflict {
		t.Fatalf("conflict code = %v (ok=%v), want %s", code, ok, gatewaywebhook.ErrorCodeReplayConflict)
	}
	waitForExecutionState(t, executions, first.ExecutionID, store.WebhookExecutionStateCompleted)
}

func TestWebhookDispatcher_OverloadLeavesNoRow(t *testing.T) {
	backend := &fakeTurnRuntime{overload: true}
	dispatcher, executions := webhookTestSetup(t, backend)

	if _, err := dispatcher.Dispatch(t.Context(), webhookTestEvent()); err == nil {
		t.Fatal("expected overload error, got nil")
	} else if code, ok := gatewaywebhook.CodeOf(err); !ok || code != gatewaywebhook.ErrorCodeRuntimeOverloaded {
		t.Fatalf("overload code = %v (ok=%v), want %s", code, ok, gatewaywebhook.ErrorCodeRuntimeOverloaded)
	}
	if _, found, err := executions.ExecutionByNonce(t.Context(), "primary", "nonce-abcdefghijklmnop"); err != nil || found {
		t.Errorf("overloaded request left an execution row: found=%v err=%v", found, err)
	}
}

func TestWebhookDispatcher_ConcurrentDuplicatesSubmitOnce(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := webhookTestSetup(t, backend)

	const callers = 8
	refs := make([]gatewaywebhook.ExecutionRef, callers)
	errs := make([]error, callers)
	var group sync.WaitGroup
	for i := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			ref, err := dispatcher.Dispatch(context.Background(), webhookTestEvent())
			refs[i] = ref
			errs[i] = err
		}()
	}
	group.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if refs[i] != refs[0] || refs[i] == (gatewaywebhook.ExecutionRef{}) {
			t.Fatalf("caller %d ref = %+v, want %+v", i, refs[i], refs[0])
		}
	}
	waitForExecutionState(t, executions, refs[0].ExecutionID, store.WebhookExecutionStateCompleted)
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
}

func TestWebhookDispatcher_OrphanedAcceptedReplayResubmits(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, executions := webhookTestSetup(t, backend)
	ctx := t.Context()

	now := time.Now().UTC()
	orphan := &store.WebhookExecution{
		ID:         "whex_orphan",
		KeyID:      "primary",
		EventID:    "evt-1",
		Nonce:      "nonce-abcdefghijklmnop",
		BodyDigest: "digest-1",
		TurnID:     "turn_orphan",
		State:      store.WebhookExecutionStateAccepted,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
	if err := executions.InsertExecution(ctx, orphan); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}

	ref, err := dispatcher.Dispatch(ctx, webhookTestEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ref.ExecutionID != "whex_orphan" || ref.TurnID != "turn_orphan" {
		t.Fatalf("replay ref = %+v, want the orphaned identity", ref)
	}
	waitForExecutionState(t, executions, "whex_orphan", store.WebhookExecutionStateCompleted)
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want 1", backend.submitted())
	}
}

func TestWebhookDispatcher_FailedTurnRecordsCode(t *testing.T) {
	backend := &fakeTurnRuntime{events: func(turnID string) []turnOutcome {
		return []turnOutcome{
			{event: store.RuntimeEvent{ID: "evt_a_" + turnID, TurnID: turnID, Kind: runtime.EventKindTurnAccepted}},
			{event: store.RuntimeEvent{ID: "evt_f_" + turnID, TurnID: turnID, Kind: runtime.EventKindTurnFailed, Payload: []byte(`{"code":"model_capability_unsupported","detail":"no"}`)}},
		}
	}}
	dispatcher, executions := webhookTestSetup(t, backend)

	ref, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	execution := waitForExecutionState(t, executions, ref.ExecutionID, store.WebhookExecutionStateFailed)
	if execution.ErrorCode != "model_capability_unsupported" {
		t.Errorf("error code = %q, want model_capability_unsupported", execution.ErrorCode)
	}
}

func githubTestStore(t *testing.T) (*sql.DB, *workflow.Store) {
	t.Helper()
	db, err := store.OpenDB(t.Context(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db, workflow.NewStore(db)
}

func githubTestBinding(t *testing.T, disk *workflow.Store, backend *durable.Fake) string {
	t.Helper()
	ctx := context.Background()
	event := "check_suite.completed"
	spec := &workflow.Spec{
		ID: "github-wait", Goal: "Wait CI", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "hold", Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &event}, Timeout: time.Minute},
		},
	}
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	summary, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	backend.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
		_, ok := inv.Signal(ctx, "wait.hold")
		if !ok {
			return context.Canceled
		}
		return nil
	})
	if _, err := backend.Start(ctx, durable.StartRequest{Handler: "waiter", Key: summary.DurableKey}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := disk.BindCorrelation(ctx, &workflow.Correlation{
		Source: "github", EventType: event, ExternalID: "org/repo#42",
		RunID: summary.ID, SignalName: "wait.hold", DedupeKey: "",
	}); err != nil {
		t.Fatalf("BindCorrelation: %v", err)
	}
	return summary.ID
}

func githubSuiteBody() []byte {
	return []byte(`{"action":"completed","repository":{"full_name":"org/repo"},` +
		`"check_suite":{"status":"completed","conclusion":"success","head_sha":"abc123",` +
		`"html_url":"https://github.com/org/repo/suites/1","pull_requests":[{"number":42}]}}`)
}

func TestDispatcherRoutesGitHubEventToCorrelation(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, _ := webhookTestSetup(t, backend)
	_, disk := githubTestStore(t)
	fake := durable.NewFake()
	runID := githubTestBinding(t, disk, fake)
	adapter, err := github.NewAdapter(disk, fake, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	dispatcher.github = adapter

	event := &gatewaywebhook.AcceptedEvent{
		KeyID: "github", Nonce: "nonce-abcdefghijklmnop", BodyDigest: "digest-github-1", Body: githubSuiteBody(),
	}
	ref, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch github event: %v", err)
	}
	if ref.ExecutionID != runID {
		t.Errorf("execution id = %q, want run %q", ref.ExecutionID, runID)
	}
	if backend.submitted() != 0 {
		t.Errorf("submitted turns = %d, want none for github events", backend.submitted())
	}
	again, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("duplicate Dispatch: %v", err)
	}
	if again.ExecutionID != runID {
		t.Errorf("duplicate execution id = %q, want run %q", again.ExecutionID, runID)
	}
}

func TestDispatcherFallsBackToTurnsForNonGitHub(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	dispatcher, _ := webhookTestSetup(t, backend)
	_, disk := githubTestStore(t)
	adapter, err := github.NewAdapter(disk, durable.NewFake(), nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	dispatcher.github = adapter

	ref, err := dispatcher.Dispatch(t.Context(), webhookTestEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ref.TurnID == "" {
		t.Error("expected a turn execution reference for non-github events")
	}
	if backend.submitted() != 1 {
		t.Errorf("submitted turns = %d, want one", backend.submitted())
	}
}
