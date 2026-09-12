package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/broadcast"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/scheduler"
	"github.com/anggasct/aura/internal/store"
)

type fakeCronEngine struct {
	mu      sync.Mutex
	calls   []*runtime.TurnRequest
	batches [][]store.RuntimeEvent
	errs    []error
}

func (f *fakeCronEngine) Run(_ context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	var batch []store.RuntimeEvent
	var err error
	if len(f.batches) > 0 {
		batch = f.batches[0]
		f.batches = f.batches[1:]
	}
	if len(f.errs) > 0 {
		err = f.errs[0]
		f.errs = f.errs[1:]
	}
	return func(yield func(store.RuntimeEvent, error) bool) {
		if err != nil {
			yield(store.RuntimeEvent{}, err)
			return
		}
		for i := range batch {
			if !yield(batch[i], nil) {
				return
			}
		}
	}
}

func cronTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenDB(t.Context(), t.TempDir()+"/cron.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func cronCompletedBatch(turnID, text string) []store.RuntimeEvent {
	return []store.RuntimeEvent{
		{Kind: runtime.EventKindTurnAccepted, TurnID: turnID},
		{Kind: "message.completed", TurnID: turnID, Payload: json.RawMessage(`{"text":"` + text + `"}`)},
		{Kind: runtime.EventKindTurnCompleted, TurnID: turnID},
	}
}

func TestEngineTurnRunnerRendersTerminalText(t *testing.T) {
	db := cronTestDB(t)
	engine := &fakeCronEngine{batches: [][]store.RuntimeEvent{cronCompletedBatch("turn-1", "nightly done")}}
	runner := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	result, err := runner.RunTurn(t.Context(), &scheduler.TurnRequest{
		SessionID: "cron:job-1", PrincipalID: "cron", Origin: "internal",
		Prompt: "summarize", IdempotencyKey: "cron:job-1:20260912T210000",
	})
	if err != nil {
		t.Fatalf("RunTurn(): %v", err)
	}
	if !result.Succeeded || result.TurnID != "turn-1" || result.Text != "nightly done" {
		t.Errorf("result = %+v", result)
	}
	if _, err := store.NewSessionService(db).Get(t.Context(), "cron:job-1"); err != nil {
		t.Errorf("cron session not ensured: %v", err)
	}
	call := engine.calls[0]
	if call.IdempotencyKey != "cron:job-1:20260912T210000" || string(call.Origin) != "internal" {
		t.Errorf("turn request = %+v", call)
	}
}

func TestEngineTurnRunnerFailedAndTruncated(t *testing.T) {
	db := cronTestDB(t)
	engine := &fakeCronEngine{batches: [][]store.RuntimeEvent{
		{
			{Kind: runtime.EventKindTurnAccepted, TurnID: "turn-9"},
			{Kind: runtime.EventKindTurnFailed, TurnID: "turn-9"},
		},
	}}
	runner := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	result, err := runner.RunTurn(t.Context(), &scheduler.TurnRequest{
		SessionID: "cron:job-9", PrincipalID: "cron", Origin: "internal",
		Prompt: "summarize", IdempotencyKey: "key-9",
	})
	if err != nil {
		t.Fatalf("RunTurn(): %v", err)
	}
	if result.Succeeded || result.TurnID != "turn-9" {
		t.Errorf("failed result = %+v", result)
	}

	starved := &fakeCronEngine{batches: [][]store.RuntimeEvent{
		{{Kind: runtime.EventKindTurnAccepted, TurnID: "turn-8"}},
	}}
	starvedRunner := &engineTurnRunner{engine: starved, sessions: store.NewSessionService(db)}
	if _, err := starvedRunner.RunTurn(t.Context(), &scheduler.TurnRequest{
		SessionID: "cron:job-8", PrincipalID: "cron", Origin: "internal",
		Prompt: "summarize", IdempotencyKey: "key-8",
	}); err == nil {
		t.Error("truncated stream accepted")
	}
}

func TestEngineTurnRunnerMapsOverload(t *testing.T) {
	db := cronTestDB(t)
	engine := &fakeCronEngine{errs: []error{&runtime.Error{Code: runtime.ErrorCodeRuntimeOverloaded, Detail: "full"}}}
	runner := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	_, err := runner.RunTurn(t.Context(), &scheduler.TurnRequest{
		SessionID: "cron:job-1", PrincipalID: "cron", Origin: "internal",
		Prompt: "summarize", IdempotencyKey: "key-1",
	})
	if err == nil {
		t.Fatal("overload swallowed")
	}
	if code, ok := scheduler.CodeOf(err); !ok || code != scheduler.ErrorCodeRuntimeOverloaded {
		t.Errorf("overload code = %v, %v", code, ok)
	}
	if _, err := runner.RunTurn(t.Context(), nil); err == nil {
		t.Error("nil request accepted")
	}
}

func testCronBroadcaster(t *testing.T, db *sql.DB, started *[]string) *broadcast.Broadcaster {
	t.Helper()
	broadcaster, err := broadcast.New(
		broadcast.Policy{
			Destinations:   map[string]string{"default": "discord:owner"},
			MaxDigestItems: 20, MaxDigestBytes: 12000,
		},
		&cronChannelRegistry{registered: map[string]bool{"discord": true}},
		&broadcastItemStore{store: store.NewBroadcastStore(db)},
		func(_ context.Context, itemID string) error {
			*started = append(*started, itemID)
			return nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("broadcast.New(): %v", err)
	}
	return broadcaster
}

func TestCronNotifierSubmitsBroadcast(t *testing.T) {
	db := cronTestDB(t)
	var started []string
	notifier := &cronNotifier{broadcaster: testCronBroadcaster(t, db, &started)}
	if err := notifier.Notify(t.Context(), &scheduler.NotifyRequest{
		Producer: "cron", Key: "cron:occ-1", Priority: "info",
		DestinationAlias: "default", Text: "all quiet",
	}); err != nil {
		t.Fatalf("Notify(): %v", err)
	}
	if len(started) != 1 {
		t.Fatalf("broadcast runs started = %v", started)
	}
	items, err := store.NewBroadcastStore(db).ListItems(t.Context(), nil, 10)
	if err != nil {
		t.Fatalf("ListItems(): %v", err)
	}
	if len(items) != 1 || items[0].State != "scheduled" {
		t.Errorf("broadcast rows = %+v", items)
	}
	if err := notifier.Notify(t.Context(), nil); err == nil {
		t.Error("nil notify accepted")
	}
	if err := (&cronNotifier{}).Notify(t.Context(), &scheduler.NotifyRequest{}); err == nil {
		t.Error("unwired notifier accepted")
	}
}

func TestScheduleStoreAdapterRoundTrip(t *testing.T) {
	db := cronTestDB(t)
	adapter := &scheduleStore{store: store.NewScheduleStore(db)}
	now := time.Now().UTC()
	if err := adapter.InsertJob(t.Context(), &scheduler.Job{
		ID: "job-1", Name: "n", CronExpression: "* * * * *", Timezone: "UTC",
		Prompt: "p", OriginChannel: "discord", OriginDestination: "default",
		OverlapPolicy: "skip", State: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertJob(): %v", err)
	}
	job, found, err := adapter.Job(t.Context(), "job-1")
	if err != nil || !found || job.Name != "n" {
		t.Errorf("Job() = %+v, %v, %v", job.ID, found, err)
	}
	if _, found, err := adapter.Job(t.Context(), "missing"); err != nil || found {
		t.Errorf("missing Job() = %v, %v", found, err)
	}
}

func TestBuildScheduleRunnerSmoke(t *testing.T) {
	db := cronTestDB(t)
	var started []string
	runner, err := buildScheduleRunner(db, slog.Default(), &fakeCronEngine{}, testCronBroadcaster(t, db, &started), 0, nil)
	if err != nil {
		t.Fatalf("buildScheduleRunner(): %v", err)
	}
	if runner == nil {
		t.Fatal("nil runner")
	}
}

type recordingScheduleRegistrar struct {
	names []string
}

func (r *recordingScheduleRegistrar) RegisterHandler(name string, _ durable.Handler) {
	r.names = append(r.names, name)
}

func TestRegisterScheduleHandler(t *testing.T) {
	db := cronTestDB(t)
	var started []string
	runner, err := buildScheduleRunner(db, slog.Default(), &fakeCronEngine{}, testCronBroadcaster(t, db, &started), 0, nil)
	if err != nil {
		t.Fatalf("buildScheduleRunner(): %v", err)
	}
	registrar := &recordingScheduleRegistrar{}
	if err := registerScheduleHandler(registrar, runner); err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(registrar.names) != 1 || registrar.names[0] != scheduler.HandlerName {
		t.Errorf("registered = %v", registrar.names)
	}
	if err := registerScheduleHandler(registrar, nil); err == nil {
		t.Error("nil runner accepted")
	}
	if err := registerScheduleHandler(struct{}{}, runner); err == nil {
		t.Error("non-registrar accepted")
	}
}

func TestResumeScheduleRuns(t *testing.T) {
	db := cronTestDB(t)
	now := time.Now().UTC()
	svc := store.NewScheduleStore(db)
	for _, id := range []string{"job-1", "job-2"} {
		if err := svc.InsertJob(t.Context(), &store.ScheduledJob{
			ID: id, Name: id, CronExpression: "* * * * *", Timezone: "UTC",
			Prompt: "p", OriginChannel: "discord", OriginDestination: "default",
			OverlapPolicy: "skip", State: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	rt := durable.NewFake()
	rt.RegisterHandler(scheduler.HandlerName, func(context.Context, durable.Invocation) error { return nil })
	count, err := resumeScheduleRuns(t.Context(), rt, db)
	if err != nil {
		t.Fatalf("resumeScheduleRuns(): %v", err)
	}
	if count != 2 {
		t.Errorf("resumed = %d, want 2", count)
	}
}
