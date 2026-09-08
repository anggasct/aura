package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/durable"
	gatewaywebhook "github.com/anggasct/aura/internal/gateway/webhook"
	"github.com/anggasct/aura/internal/workflow"
)

func suiteBody(t *testing.T, action string, pull int, sha string) []byte {
	t.Helper()
	pulls := "[]"
	if pull > 0 {
		pulls = `[{"number":42}]`
	}
	body := `{"action":"` + action + `","repository":{"full_name":"org/repo"},` +
		`"check_suite":{"status":"completed","conclusion":"success","head_sha":"` + sha + `",` +
		`"html_url":"https://github.com/org/repo/suites/1","pull_requests":` + pulls + `}}`
	if !json.Valid([]byte(body)) {
		t.Fatalf("fixture body is not valid JSON: %s", body)
	}
	return []byte(body)
}

type fakeCorrelations struct {
	mu   sync.Mutex
	rows map[string]workflow.Correlation
	runs map[string]*workflow.RunSummary
}

func correlationKey(source, eventType, externalID, dedupeKey string) string {
	return strings.Join([]string{source, eventType, externalID, dedupeKey}, "\x00")
}

func (f *fakeCorrelations) BindCorrelation(_ context.Context, correlation *workflow.Correlation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := correlationKey(correlation.Source, correlation.EventType, correlation.ExternalID, correlation.DedupeKey)
	if _, exists := f.rows[key]; exists {
		return &workflow.Error{Code: workflow.ErrorCodeCorrelationConflict, Detail: "duplicate"}
	}
	if f.rows == nil {
		f.rows = map[string]workflow.Correlation{}
	}
	f.rows[key] = *correlation
	return nil
}

func (f *fakeCorrelations) ResolveCorrelation(_ context.Context, source, eventType, externalID, dedupeKey string) (workflow.Correlation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Source == source && row.EventType == eventType && row.ExternalID == externalID && row.DedupeKey == dedupeKey {
			return row, nil
		}
	}
	return workflow.Correlation{}, &workflow.Error{Code: workflow.ErrorCodeCorrelationUnmatched, Detail: "no match"}
}

func (f *fakeCorrelations) DeleteCorrelation(_ context.Context, source, eventType, externalID, dedupeKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := correlationKey(source, eventType, externalID, dedupeKey)
	if _, exists := f.rows[key]; !exists {
		return &workflow.Error{Code: workflow.ErrorCodeCorrelationUnmatched, Detail: "nothing to delete"}
	}
	delete(f.rows, key)
	return nil
}

func (f *fakeCorrelations) Run(_ context.Context, runID string) (*workflow.RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	summary, ok := f.runs[runID]
	if !ok {
		return nil, &workflow.Error{Code: workflow.ErrorCodeRunNotFound, Detail: "no such run"}
	}
	return summary, nil
}

func (f *fakeCorrelations) deliveryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, row := range f.rows {
		if row.DedupeKey != "" {
			count++
		}
	}
	return count
}

type countingRuntime struct {
	durable.Runtime
	mu      sync.Mutex
	signals int
	fail    error
}

func (c *countingRuntime) Signal(ctx context.Context, run durable.RunRef, name string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.signals++
	return c.Runtime.Signal(ctx, run, name, payload)
}

func (c *countingRuntime) setFail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = err
}

func (c *countingRuntime) signalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signals
}

func testAdapter(t *testing.T, store *fakeCorrelations, runtime *countingRuntime) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(store, runtime, nil)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return adapter
}

func seedBinding(store *fakeCorrelations, runID, durableKey string) {
	if store.runs == nil {
		store.runs = map[string]*workflow.RunSummary{}
	}
	store.runs[runID] = &workflow.RunSummary{ID: runID, DurableKey: durableKey}
	_ = store.BindCorrelation(context.Background(), &workflow.Correlation{
		Source: "github", EventType: "check_suite.completed", ExternalID: "org/repo#42",
		RunID: runID, SignalName: "wait.hold", DedupeKey: "",
	})
}

func acceptedEvent(body []byte, digest string) *gatewaywebhook.AcceptedEvent {
	return &gatewaywebhook.AcceptedEvent{KeyID: "github", Nonce: "nonce-abcdefghijklmnop", BodyDigest: digest, Body: body}
}

func TestNormalizeCheckSuiteWithPullRequest(t *testing.T) {
	event, isGitHub, err := Normalize(suiteBody(t, "completed", 42, "abc123"))
	if err != nil || !isGitHub {
		t.Fatalf("Normalize = %+v, %v, %v; want github event", event, isGitHub, err)
	}
	if event.EventType != "check_suite.completed" || event.ExternalID != "org/repo#42" {
		t.Errorf("normalized = %+v, want suite completion for org/repo#42", event)
	}
	if !strings.Contains(string(event.Payload), `"conclusion":"success"`) {
		t.Errorf("payload = %s, want the conclusion", event.Payload)
	}
}

func TestNormalizeFallsBackToCommitSHA(t *testing.T) {
	event, isGitHub, err := Normalize(suiteBody(t, "completed", 0, "abc123def456"))
	if err != nil || !isGitHub {
		t.Fatalf("Normalize = %+v, %v, %v; want github event", event, isGitHub, err)
	}
	if event.ExternalID != "org/repo@abc123def456" {
		t.Errorf("external id = %q, want commit form", event.ExternalID)
	}
}

func TestNormalizeRejectsNonGitHubBodies(t *testing.T) {
	for name, body := range map[string][]byte{
		"invalid json": []byte(`{oops`),
		"turn payload": []byte(`{"k":1}`),
		"no suite":     []byte(`{"action":"completed","repository":{"full_name":"org/repo"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, isGitHub, err := Normalize(body)
			if err != nil || isGitHub {
				t.Errorf("Normalize = %v, %v; want non-github", isGitHub, err)
			}
		})
	}
}

func TestNormalizeRejectsMalformedGitHubBodies(t *testing.T) {
	_, isGitHub, err := Normalize([]byte(`{"action":"","repository":{"full_name":"org/repo"},"check_suite":{}}`))
	if err == nil || !isGitHub {
		t.Errorf("expected invalid empty action to fail, got %v, %v", isGitHub, err)
	}
}

func TestHandleIgnoresNonGitHubEvents(t *testing.T) {
	store := &fakeCorrelations{}
	runtime := &countingRuntime{Runtime: durable.NewFake()}
	adapter := testAdapter(t, store, runtime)
	handled, _, err := adapter.Handle(context.Background(), acceptedEvent([]byte(`{"k":1}`), "digest-1"))
	if err != nil || handled {
		t.Errorf("Handle = %v, %v; want passthrough", handled, err)
	}
}

func TestHandleUnmatchedEventAcknowledgesWithoutSignal(t *testing.T) {
	store := &fakeCorrelations{}
	runtime := &countingRuntime{Runtime: durable.NewFake()}
	adapter := testAdapter(t, store, runtime)
	handled, _, err := adapter.Handle(context.Background(), acceptedEvent(suiteBody(t, "completed", 42, "abc123"), "digest-1"))
	if err != nil || !handled {
		t.Fatalf("Handle = %v, %v; want acknowledged", handled, err)
	}
	if runtime.signalCount() != 0 {
		t.Errorf("signals = %d, want none for unmatched events", runtime.signalCount())
	}
	if store.deliveryCount() != 0 {
		t.Errorf("deliveries = %d, want none recorded", store.deliveryCount())
	}
}

func TestHandleAdvancesRunExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := &fakeCorrelations{}
	backend := durable.NewFake()
	backend.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
		_, ok := inv.Signal(ctx, "wait.hold")
		if !ok {
			return context.Canceled
		}
		return nil
	})
	if _, err := backend.Start(ctx, durable.StartRequest{Handler: "waiter", Key: "durable-1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runtime := &countingRuntime{Runtime: backend}
	adapter := testAdapter(t, store, runtime)
	seedBinding(store, "run-1", "durable-1")
	event := acceptedEvent(suiteBody(t, "completed", 42, "abc123"), "digest-1")
	handled, ref, err := adapter.Handle(ctx, event)
	if err != nil || !handled || ref.ExecutionID != "run-1" {
		t.Fatalf("Handle = %v, %+v, %v; want run-1", handled, ref, err)
	}
	again, _, err := adapter.Handle(ctx, event)
	if err != nil || !again {
		t.Fatalf("duplicate Handle = %v, %v; want acknowledged", again, err)
	}
	if runtime.signalCount() != 1 {
		t.Errorf("signals = %d, want exactly one across duplicate deliveries", runtime.signalCount())
	}
}

func TestHandleSignalFailureRollsBackDelivery(t *testing.T) {
	ctx := context.Background()
	store := &fakeCorrelations{}
	backend := durable.NewFake()
	backend.RegisterHandler("waiter", func(ctx context.Context, inv durable.Invocation) error {
		_, ok := inv.Signal(ctx, "wait.hold")
		if !ok {
			return context.Canceled
		}
		return nil
	})
	if _, err := backend.Start(ctx, durable.StartRequest{Handler: "waiter", Key: "durable-1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runtime := &countingRuntime{Runtime: backend, fail: errors.New("runtime down")}
	adapter := testAdapter(t, store, runtime)
	seedBinding(store, "run-1", "durable-1")
	event := acceptedEvent(suiteBody(t, "completed", 42, "abc123"), "digest-1")
	if _, _, err := adapter.Handle(ctx, event); err == nil {
		t.Fatal("expected signal failure to surface, got nil")
	}
	if store.deliveryCount() != 0 {
		t.Fatal("failed signal left a delivery row behind; retries would treat it as duplicate")
	}
	runtime.setFail(nil)
	if _, _, err := adapter.Handle(ctx, event); err != nil {
		t.Fatalf("retry Handle: %v", err)
	}
	if runtime.signalCount() != 1 || store.deliveryCount() != 1 {
		t.Errorf("signals = %d, deliveries = %d; want one each after retry", runtime.signalCount(), store.deliveryCount())
	}
}
