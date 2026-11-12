package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

type fakeCronEngine struct {
	mu      sync.Mutex
	calls   []*turn.TurnRequest
	batches [][]store.RuntimeEvent
	errs    []error
}

func (f *fakeCronEngine) Run(_ context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
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
	return storetest.OpenMigrated(t)
}

func cronCompletedBatch(turnID, text string) []store.RuntimeEvent {
	return []store.RuntimeEvent{
		{Kind: turn.EventKindTurnAccepted, TurnID: turnID},
		{Kind: "message.completed", TurnID: turnID, Payload: json.RawMessage(`{"text":"` + text + `"}`)},
		{Kind: turn.EventKindTurnCompleted, TurnID: turnID},
	}
}

func TestEngineTurnRunnerRendersTerminalText(t *testing.T) {
	db := cronTestDB(t)
	engine := &fakeCronEngine{batches: [][]store.RuntimeEvent{cronCompletedBatch("turn-1", "nightly done")}}
	runner := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	result, err := runner.RunTurn(t.Context(), &schedule.TurnRequest{
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
			{Kind: turn.EventKindTurnAccepted, TurnID: "turn-9"},
			{Kind: turn.EventKindTurnFailed, TurnID: "turn-9"},
		},
	}}
	runner := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	result, err := runner.RunTurn(t.Context(), &schedule.TurnRequest{
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
		{{Kind: turn.EventKindTurnAccepted, TurnID: "turn-8"}},
	}}
	starvedRunner := &engineTurnRunner{engine: starved, sessions: store.NewSessionService(db)}
	if _, err := starvedRunner.RunTurn(t.Context(), &schedule.TurnRequest{
		SessionID: "cron:job-8", PrincipalID: "cron", Origin: "internal",
		Prompt: "summarize", IdempotencyKey: "key-8",
	}); err == nil {
		t.Error("truncated stream accepted")
	}
}

func TestEngineTurnRunnerMapsOverload(t *testing.T) {
	db := cronTestDB(t)
	engine := &fakeCronEngine{errs: []error{&turn.Error{Code: turn.ErrorCodeRuntimeOverloaded, Detail: "full"}}}
	runner := &engineTurnRunner{engine: engine, sessions: store.NewSessionService(db)}
	_, err := runner.RunTurn(t.Context(), &schedule.TurnRequest{
		SessionID: "cron:job-1", PrincipalID: "cron", Origin: "internal",
		Prompt: "summarize", IdempotencyKey: "key-1",
	})
	if err == nil {
		t.Fatal("overload swallowed")
	}
	if code, ok := schedule.CodeOf(err); !ok || code != schedule.ErrorCodeRuntimeOverloaded {
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
	if err := notifier.Notify(t.Context(), &schedule.NotifyRequest{
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
	if err := (&cronNotifier{}).Notify(t.Context(), &schedule.NotifyRequest{}); err == nil {
		t.Error("unwired notifier accepted")
	}
}

func TestScheduleStoreAdapterRoundTrip(t *testing.T) {
	db := cronTestDB(t)
	adapter := &scheduleStore{store: store.NewScheduleStore(db)}
	now := time.Now().UTC()
	if err := adapter.InsertJob(t.Context(), &schedule.Job{
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
	runner, err := buildScheduleRunner(&scheduleRunnerInput{db: db, logger: slog.Default(), engine: &fakeCronEngine{}, broadcaster: testCronBroadcaster(t, db, &started), starter: durable.NewFake()})
	if err != nil {
		t.Fatalf("buildScheduleRunner(): %v", err)
	}
	if runner == nil {
		t.Fatal("nil runner")
	}
	if _, err := buildScheduleRunner(nil); err == nil {
		t.Error("nil input accepted")
	}
	if _, err := buildScheduleRunner(&scheduleRunnerInput{db: db, logger: slog.Default(), engine: &fakeCronEngine{}, broadcaster: testCronBroadcaster(t, db, &started)}); err == nil {
		t.Error("runner without a starter accepted")
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
	runner, err := buildScheduleRunner(&scheduleRunnerInput{db: db, logger: slog.Default(), engine: &fakeCronEngine{}, broadcaster: testCronBroadcaster(t, db, &started), starter: durable.NewFake()})
	if err != nil {
		t.Fatalf("buildScheduleRunner(): %v", err)
	}
	registrar := &recordingScheduleRegistrar{}
	if err := registerScheduleHandler(registrar, runner); err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(registrar.names) != 1 || registrar.names[0] != schedule.HandlerName {
		t.Errorf("registered = %v", registrar.names)
	}
	if err := registerScheduleHandler(registrar, nil); err == nil {
		t.Error("nil runner accepted")
	}
	if err := registerScheduleHandler(struct{}{}, runner); err == nil {
		t.Error("non-registrar accepted")
	}
}

const (
	scheduleWaitDeadline = 10 * time.Second
	scheduleWaitTick     = 2 * time.Millisecond
	dailySchedule        = "0 21 * * *"
)

func awaitSchedule(t *testing.T, what string, reached func() bool) {
	t.Helper()
	deadline := time.NewTimer(scheduleWaitDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(scheduleWaitTick)
	defer tick.Stop()
	for !reached() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func scheduleRunKey(jobID string, instant time.Time) string {
	return "schedule/" + jobID + "/" + instant.UTC().Format("20060102T150405.000000000Z")
}

func runExists(t *testing.T, rt *durable.Fake, key string) bool {
	t.Helper()
	_, err := rt.Status(t.Context(), durable.RunRef{Key: key})
	return err == nil
}

type scheduleSeed struct {
	id    string
	state string
	fired time.Time
}

func seedScheduleJobs(t *testing.T, db *sql.DB, seeds []scheduleSeed) {
	t.Helper()
	now := time.Now().UTC()
	jobs := store.NewScheduleStore(db)
	for _, seed := range seeds {
		if err := jobs.InsertJob(t.Context(), &store.ScheduledJob{
			ID: seed.id, Name: seed.id, CronExpression: dailySchedule, Timezone: "UTC",
			Prompt: "p", OriginChannel: "discord", OriginDestination: "default",
			OverlapPolicy: "skip", CatchUpGraceSeconds: 900, State: store.ScheduleJobActive, Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
		if !seed.fired.IsZero() {
			if err := jobs.RecordFire(t.Context(), &store.ScheduledOccurrence{
				ID: "cron-" + seed.id, JobID: seed.id, JobVersion: 1, ScheduledForUTC: seed.fired,
				State: store.OccurrenceFired, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed the fired occurrence of %s: %v", seed.id, err)
			}
		}
		if seed.state != store.ScheduleJobActive {
			if err := jobs.SetJobState(t.Context(), seed.id, seed.state, now); err != nil {
				t.Fatalf("set the state of %s: %v", seed.id, err)
			}
		}
	}
}

func nextDailyFires(t *testing.T, before, after time.Time) []time.Time {
	t.Helper()
	parsed, err := schedule.ParseExpression(dailySchedule)
	if err != nil {
		t.Fatalf("ParseExpression(): %v", err)
	}
	var fires []time.Time
	for _, from := range []time.Time{before, after} {
		fire, err := schedule.NextFire(&parsed, time.UTC, from)
		if err != nil {
			t.Fatalf("NextFire(): %v", err)
		}
		fires = append(fires, fire)
	}
	return fires
}

func nextFireKeys(jobID string, fires []time.Time) []string {
	keys := make([]string, 0, len(fires))
	for _, fire := range fires {
		keys = append(keys, scheduleRunKey(jobID, fire))
	}
	return keys
}

func anyRunExists(t *testing.T, rt *durable.Fake, keys []string) bool {
	t.Helper()
	return slices.ContainsFunc(keys, func(key string) bool { return runExists(t, rt, key) })
}

func TestResumeScheduleRuns(t *testing.T) {
	db := cronTestDB(t)
	yesterday := time.Now().UTC().Truncate(24 * time.Hour).Add(-3 * time.Hour)
	manual := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(7 * time.Nanosecond)
	seedScheduleJobs(t, db, []scheduleSeed{
		{id: "job-idle", state: store.ScheduleJobActive},
		{id: "job-busy", state: store.ScheduleJobActive, fired: yesterday},
		{id: "job-paused-fired", state: store.ScheduleJobPaused, fired: manual},
		{id: "job-paused", state: store.ScheduleJobPaused},
		{id: "job-deleted", state: store.ScheduleJobDeleted},
	})
	rt := durable.NewFake()
	rt.RegisterHandler(schedule.HandlerName, func(context.Context, durable.Invocation) error { return nil })
	before := time.Now().UTC()
	count, err := resumeScheduleRuns(t.Context(), rt, db)
	if err != nil {
		t.Fatalf("resumeScheduleRuns(): %v", err)
	}
	fires := nextDailyFires(t, before, time.Now().UTC())
	if count != 3 {
		t.Errorf("started = %d, want 3", count)
	}
	for _, key := range []string{scheduleRunKey("job-busy", yesterday), scheduleRunKey("job-paused-fired", manual)} {
		if !runExists(t, rt, key) {
			t.Errorf("no recorded run %s", key)
		}
	}
	if !anyRunExists(t, rt, nextFireKeys("job-idle", fires)) {
		t.Errorf("no run for the next fire of the idle job (%v)", fires)
	}
	for _, jobID := range []string{"job-busy", "job-paused-fired", "job-paused", "job-deleted"} {
		if anyRunExists(t, rt, nextFireKeys(jobID, fires)) {
			t.Errorf("%s got a run for its next fire", jobID)
		}
	}

	if _, err := resumeScheduleRuns(t.Context(), durable.NewFake(), db); err == nil {
		t.Error("a failed start was not returned")
	}
}

type passLogs struct {
	mu       sync.Mutex
	warnings []string
}

func (l *passLogs) Enabled(context.Context, slog.Level) bool { return true }

func (l *passLogs) Handle(_ context.Context, record slog.Record) error {
	if record.Level != slog.LevelWarn {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, record.Message)
	return nil
}

func (l *passLogs) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *passLogs) WithGroup(string) slog.Handler { return l }

func (l *passLogs) warned() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.warnings, "schedule pass failed")
}

func TestSchedulePassPicksUpAManualOccurrence(t *testing.T) {
	db := cronTestDB(t)
	seedScheduleJobs(t, db, []scheduleSeed{{id: "job-1", state: store.ScheduleJobActive}})
	rt := durable.NewFake()
	logs := &passLogs{}
	pass := &schedulePass{runtime: rt, db: db, logger: slog.New(logs), interval: 5 * time.Millisecond}
	before := time.Now().UTC()
	task := server.Background("schedule_pass", pass.run)
	if err := task.Start(t.Context()); err != nil {
		t.Fatalf("start the pass: %v", err)
	}
	awaitSchedule(t, "a failed pass is logged", logs.warned)

	rt.RegisterHandler(schedule.HandlerName, func(context.Context, durable.Invocation) error { return nil })
	awaitSchedule(t, "a later pass starts the next fire", func() bool {
		return anyRunExists(t, rt, nextFireKeys("job-1", nextDailyFires(t, before, time.Now().UTC())))
	})

	service, err := scheduleService(db)
	if err != nil {
		t.Fatalf("scheduleService(): %v", err)
	}
	occurrence, err := service.RunNow(t.Context(), "job-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("RunNow(): %v", err)
	}
	awaitSchedule(t, "a later pass starts the manual occurrence", func() bool {
		return runExists(t, rt, scheduleRunKey("job-1", occurrence.ScheduledForUTC))
	})
	if err := task.Stop(t.Context()); err != nil {
		t.Errorf("stop the pass: %v", err)
	}
}
