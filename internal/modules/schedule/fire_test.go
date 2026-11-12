package schedule

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStartNextUsesTheJobsExpressionAndTimezone(t *testing.T) {
	starter := &recordingStarter{}
	job := &Job{ID: "job-1", CronExpression: "0 9 * * *", Timezone: "Asia/Jakarta"}
	after := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	if err := StartNext(t.Context(), &NextRequest{Starter: starter, Job: job, After: after}); err != nil {
		t.Fatalf("StartNext(): %v", err)
	}
	if got, want := starter.keys(), []string{"schedule/job-1/20260912T020000.000000000Z"}; !slices.Equal(got, want) {
		t.Errorf("started = %v, want %v", got, want)
	}
	payloads := starter.payloads(t)
	want := firePayload{JobID: "job-1", Instant: time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC), Chain: true, Expression: "0 9 * * *", Timezone: "Asia/Jakarta"}
	if len(payloads) != 1 || payloads[0] != want {
		t.Errorf("payloads = %+v, want %+v", payloads, want)
	}
}

type passJob struct {
	id    string
	state string
	fired time.Time
}

func seedPass(t *testing.T, store *fakeJobStore, jobs []passJob) {
	t.Helper()
	for _, seed := range jobs {
		if err := store.InsertJob(t.Context(), &Job{
			ID: seed.id, Name: seed.id, CronExpression: "0 21 * * *", Timezone: "UTC",
			Prompt: "p", OriginChannel: "discord", OriginDestination: "default",
			OverlapPolicy: OverlapSkip, CatchUpGraceSeconds: 900, State: seed.state, Version: 1,
		}); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
		if !seed.fired.IsZero() {
			store.seed(&Occurrence{ID: "cron-" + seed.id, JobID: seed.id, ScheduledForUTC: seed.fired, State: OccurrenceFired})
		}
	}
}

func TestStartRunsStartsFiredOccurrencesAndNextFires(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	scheduled := time.Date(2026, 9, 11, 21, 0, 0, 0, time.UTC)
	manual := time.Date(2026, 9, 12, 11, 0, 0, 5, time.UTC)
	store := newFakeJobStore()
	seedPass(t, store, []passJob{
		{id: "job-0-refused", state: JobActive},
		{id: "job-busy", state: JobActive, fired: scheduled},
		{id: "job-idle", state: JobActive},
		{id: "job-paused-fired", state: JobPaused, fired: manual},
		{id: "job-paused", state: JobPaused},
		{id: "job-deleted", state: JobDeleted},
	})
	starter := &recordingStarter{fail: "job-0-refused"}
	started, err := StartRuns(t.Context(), &RunsRequest{Starter: starter, Store: store, Now: now})
	if err == nil || !strings.Contains(err.Error(), "start refused") {
		t.Errorf("StartRuns() error = %v, want the refused start", err)
	}
	next := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	want := []string{fireKey("job-busy", scheduled), fireKey("job-paused-fired", manual), fireKey("job-idle", next)}
	if got := starter.keys(); !slices.Equal(got, want) || started != len(want) {
		t.Errorf("started %d: %v, want %v", started, got, want)
	}
	chains := []bool{}
	for _, payload := range starter.payloads(t) {
		chains = append(chains, payload.Chain)
	}
	if !slices.Equal(chains, []bool{false, false, true}) {
		t.Errorf("chain flags = %v, want two recorded runs and one scheduled run", chains)
	}
	if _, err := StartRuns(t.Context(), &RunsRequest{Store: store, Now: now}); err == nil {
		t.Error("a pass without a starter was accepted")
	}
}
