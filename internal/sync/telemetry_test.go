package sync

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMetricsRecord(t *testing.T) {
	t.Parallel()
	var metrics Metrics
	metrics.Record(Observation{Result: string(AdvanceUpToDate), Latency: time.Second, EntryCount: 3, ByteCount: 512})
	metrics.Record(Observation{Result: string(AdvanceConflict), Conflict: true, Latency: 2 * time.Second})
	metrics.Record(Observation{Result: string(WorkerUnknown), Unknown: true})
	if metrics.Counters.Runs != 3 {
		t.Fatalf("runs = %d, want 3", metrics.Counters.Runs)
	}
	if metrics.Counters.Conflicts != 1 || metrics.Counters.Unknowns != 1 {
		t.Fatalf("counters = %+v, want one conflict and one unknown", metrics.Counters)
	}
	if metrics.LastResult != string(WorkerUnknown) {
		t.Fatalf("last result = %q, want unknown", metrics.LastResult)
	}
}

func TestStateStoreTransitions(t *testing.T) {
	t.Parallel()
	store := NewStateStore()
	now := time.Now().UTC()
	store.MarkRunning()
	if got := store.Load(); got.State != WorkerRunning {
		t.Fatalf("state = %q, want running", got.State)
	}
	store.MarkIdle("local-1", "remote-1", now, time.Second, string(AdvanceFastForward))
	got := store.Load()
	if got.State != WorkerIdle || got.LocalRef != "local-1" || got.RemoteRef != "remote-1" || got.Runs != 1 {
		t.Fatalf("checkpoint = %+v, want idle with refs", got)
	}
	store.MarkConflict("", "", now)
	if got := store.Load(); got.State != WorkerConflict || got.LocalRef != "local-1" {
		t.Fatalf("conflict must keep last refs: %+v", got)
	}
	store.MarkUnknown("intent-1", now)
	if got := store.Load(); got.State != WorkerUnknown || got.UnknownID != "intent-1" {
		t.Fatalf("checkpoint = %+v, want unknown with intent", got)
	}
	store.ClearUnknown("local-2", "remote-2", now, string(AdvanceUpToDate))
	if got := store.Load(); got.State != WorkerIdle || got.UnknownID != "" {
		t.Fatalf("checkpoint = %+v, want cleared unknown", got)
	}
	store.MarkDisabled()
	if got := store.Load(); got.State != WorkerDisabled {
		t.Fatalf("state = %q, want disabled", got.State)
	}
}

func TestObservationHidesContent(t *testing.T) {
	t.Parallel()
	observation := Observation{Result: string(AdvanceConflict), Conflict: true}
	var metrics Metrics
	metrics.Record(observation)
	if strings.Contains(metrics.Snapshot().LastResult, "skills/") {
		t.Fatal("metrics must not carry paths")
	}
}

func TestMetricsAllowlistNormalizes(t *testing.T) {
	t.Parallel()
	var metrics Metrics
	metrics.Record(Observation{Result: "skills/reviewed/evil/SKILL.md", Latency: time.Second})
	if got := metrics.Snapshot().LastResult; got != string(WorkerUnknown) {
		t.Fatalf("last result = %q, want unknown for free-form", got)
	}
	if got := metrics.Snapshot().Counters.Runs; got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

func TestWorkerMetricsBuckets(t *testing.T) {
	t.Parallel()
	outcomes := []PassOutcome{
		{State: WorkerIdle, Result: string(AdvanceUpToDate), LocalRef: "local-1", RemoteRef: "remote-1"},
		{State: WorkerConflict, Conflict: true, LocalRef: "local-1", RemoteRef: "remote-1", Result: string(AdvanceConflict)},
		{State: WorkerUnknown, UnknownID: "intent-1", Result: string(WorkerUnknown), Retryable: true},
	}
	index := 0
	state := NewStateStore()
	worker, err := NewWorker(WorkerConfig{
		Interval: time.Hour,
		Clock:    time.Now().UTC,
		Jitter:   func() float64 { return 0.5 },
	}, state, func(_ context.Context) PassOutcome {
		outcome := outcomes[index]
		if index < len(outcomes)-1 {
			index++
		}
		return outcome
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	for range outcomes {
		worker.RunOnce(context.Background())
	}
	snapshot := worker.MetricsSnapshot()
	if snapshot.Counters.Runs != 3 {
		t.Fatalf("runs = %d, want 3", snapshot.Counters.Runs)
	}
	if snapshot.Counters.Conflicts != 1 || snapshot.Counters.Unknowns != 1 {
		t.Fatalf("counters = %+v, want one conflict and one unknown", snapshot.Counters)
	}
	if got := state.Load(); got.Runs != 3 {
		t.Fatalf("state runs = %d, want 3", got.Runs)
	}
}

func TestFileStateStorePersists(t *testing.T) {
	t.Parallel()
	path := SyncStatePath(t.TempDir())
	first, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	now := time.Now().UTC()
	first.MarkConflict("local-1", "remote-1", now)
	second, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := second.Load(); got.State != WorkerConflict || got.LocalRef != "local-1" {
		t.Fatalf("conflict not durable: %+v", got)
	}
	second.MarkUnknown("intent-9", now)
	third, err := NewFileStateStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := third.Load(); got.State != WorkerUnknown || got.UnknownID != "intent-9" {
		t.Fatalf("unknown not durable: %+v", got)
	}
}
