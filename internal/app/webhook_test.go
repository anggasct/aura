package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"iter"
	"log/slog"
	"sync"
	"testing"
	"time"

	gatewaywebhook "github.com/anggasct/aura/internal/channels/webhook"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
	"github.com/anggasct/aura/internal/store/storetest"
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

func acceptedTurnEvents(turnID string) []turnOutcome {
	return []turnOutcome{
		{event: turn.Event{ID: "evt_accepted_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnAccepted}},
		{event: turn.Event{ID: "evt_msg_" + turnID, TurnID: turnID, Kind: turn.EventKindMessageCompleted}},
		{event: turn.Event{ID: "evt_done_" + turnID, TurnID: turnID, Kind: turn.EventKindTurnCompleted}},
	}
}

func webhookTestSetup(t *testing.T, backend *fakeTurnRuntime, handlers ...gatewaywebhook.EventHandler) (*gatewaywebhook.Dispatcher, platformwebhook.ExecutionStore) {
	t.Helper()
	db := storetest.OpenMigrated(t)
	executions, err := platformwebhook.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	backend.store = turnstore.NewEventStore(db)
	backend.sequences = make(map[string]uint64)
	dispatcher, err := gatewaywebhook.NewDispatcher(&gatewaywebhook.DispatcherDeps{
		Sessions:   turnstore.NewSessionService(db),
		Events:     turnstore.NewTurnStore(db),
		Executions: executions,
		Runtime:    backend,
		Retention:  24 * time.Hour,
		Clock:      time.Now,
		Logger:     slog.New(slog.DiscardHandler),
		Handlers:   handlers,
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

func webhookTestEvent() *platformwebhook.AcceptedEvent {
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

func githubTestStore(t *testing.T) (*sql.DB, *workflow.Store) {
	t.Helper()
	db := storetest.OpenMigrated(t)
	return db, mustWorkflowStore(t, db)
}

type queuedRuntime struct{ durable.Runtime }

func (queuedRuntime) Start(_ context.Context, req durable.StartRequest) (durable.RunRef, error) {
	return durable.RunRef{Key: req.Key}, nil
}

func githubTestBinding(t *testing.T, disk *workflow.Store, backend *durabletest.Fake) string {
	t.Helper()
	ctx := context.Background()
	event := "check_suite.completed"
	spec := &workflow.Spec{
		ID: "github-wait", Goal: "Wait CI", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "hold", Executor: workflow.ExecutorSpec{Kind: workflow.KindWait, Event: &event}, Timeout: time.Minute},
		},
	}
	interpreter := mustWorkflowInterpreter(t, disk, queuedRuntime{backend}, nil)
	if err := interpreter.Install(ctx, []*workflow.Spec{spec}, &workflow.ValidationDeps{Agents: builtinRegistry(t), CheckToolCall: acceptToolCall, StepAllowed: acceptStepAllowed}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	summary, err := interpreter.StartConfigured(ctx, spec.ID, nil, principal.OwnerID, principal.ClassOwner, "")
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
	_, disk := githubTestStore(t)
	fake := durabletest.NewFake()
	runID := githubTestBinding(t, disk, fake)
	adapter, err := github.NewAdapter(newWorkflowCorrelationStore(disk), fake, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	dispatcher, _ := webhookTestSetup(t, backend, adapter)

	event := &platformwebhook.AcceptedEvent{
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
	_, disk := githubTestStore(t)
	adapter, err := github.NewAdapter(newWorkflowCorrelationStore(disk), durabletest.NewFake(), nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	dispatcher, _ := webhookTestSetup(t, backend, adapter)

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

type signalCountingRuntime struct {
	durable.Runtime
	mu      sync.Mutex
	signals int
}

func (c *signalCountingRuntime) Signal(ctx context.Context, run durable.RunRef, name string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals++
	return c.Runtime.Signal(ctx, run, name, payload)
}

func (c *signalCountingRuntime) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signals
}

func envelopeWrappedGitHubEvent(t *testing.T, eventID, nonce string, payload []byte) *platformwebhook.AcceptedEvent {
	t.Helper()
	document, err := json.Marshal(platformwebhook.Envelope{
		EventID: eventID, Subject: "github delivery", Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	parsed, err := platformwebhook.ParseEnvelope(document)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	sum := sha256.Sum256(document)
	return &platformwebhook.AcceptedEvent{
		KeyID: "github", Nonce: nonce,
		BodyDigest: hex.EncodeToString(sum[:]), Body: document, Envelope: parsed,
	}
}

func TestDispatcherRoutesEnvelopeWrappedGitHubEventToCorrelation(t *testing.T) {
	backend := &fakeTurnRuntime{events: acceptedTurnEvents}
	_, disk := githubTestStore(t)
	fake := durabletest.NewFake()
	runID := githubTestBinding(t, disk, fake)
	counter := &signalCountingRuntime{Runtime: fake}
	adapter, err := github.NewAdapter(newWorkflowCorrelationStore(disk), counter, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	dispatcher, _ := webhookTestSetup(t, backend, adapter)

	event := envelopeWrappedGitHubEvent(t, "evt-github-1", "nonce-abcdefghijklmnop", githubSuiteBody())
	ref, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch envelope github event: %v", err)
	}
	if ref.ExecutionID != runID {
		t.Errorf("execution id = %q, want run %q", ref.ExecutionID, runID)
	}
	if backend.submitted() != 0 {
		t.Errorf("submitted turns = %d, want none for envelope github events", backend.submitted())
	}
	if counter.count() != 1 {
		t.Errorf("signals = %d, want exactly one for envelope github event", counter.count())
	}
	reserialized := &platformwebhook.AcceptedEvent{
		KeyID: "github", Nonce: "nonce-abcdefghijklmnop",
		BodyDigest: "different-digest-for-reserialized-envelope",
		Body:       []byte(`{"event_id":"evt-github-1","subject":"github delivery","payload":{}}`),
		Envelope:   event.Envelope,
	}
	again, err := dispatcher.Dispatch(context.Background(), reserialized)
	if err != nil {
		t.Fatalf("redelivery Dispatch: %v", err)
	}
	if again.ExecutionID != runID {
		t.Errorf("redelivery execution id = %q, want run %q", again.ExecutionID, runID)
	}
	if backend.submitted() != 0 {
		t.Errorf("submitted turns after redelivery = %d, want none", backend.submitted())
	}
	if counter.count() != 1 {
		t.Errorf("signals after redelivery = %d, want exactly one", counter.count())
	}
}
