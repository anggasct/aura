package sync

import (
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
	if strings.Contains(metrics.LastResult, "skills/") {
		t.Fatal("metrics must not carry paths")
	}
}
