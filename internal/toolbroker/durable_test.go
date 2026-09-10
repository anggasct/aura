package toolbroker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/durable"
)

type recordingSink struct {
	mu     sync.Mutex
	events []ApprovalEvent
	ready  chan struct{}
}

func (s *recordingSink) EmitApprovalRequired(_ context.Context, event *ApprovalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, *event)
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
	return nil
}

func (s *recordingSink) snapshot() []ApprovalEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ApprovalEvent(nil), s.events...)
}

func parkBroker(t *testing.T, approve bool) (backend *durable.Fake, sink *recordingSink, callTotal *int, finish func()) {
	t.Helper()
	policy := DefaultPolicy()
	readRule := policy.Rules["read_file"]
	readRule.RequiresApproval = true
	policy.Rules["read_file"] = readRule
	var calls int
	broker, err := New(&Options{
		Policy: policy,
		Adapters: map[string]Adapter{
			"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: []byte(`{"content":"x"}`)}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	backend = durable.NewFake()
	invCh := make(chan durable.Invocation, 1)
	backend.RegisterHandler("driver", func(_ context.Context, inv durable.Invocation) error {
		invCh <- inv
		select {}
	})
	ref, err := backend.Start(context.Background(), durable.StartRequest{Handler: "driver", Key: "turn-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = ref
	var inv durable.Invocation
	select {
	case inv = <-invCh:
	case <-time.After(5 * time.Second):
		t.Fatal("driver invocation never started")
	}
	sink = &recordingSink{ready: make(chan struct{})}
	ctx := WithApprovalSink(context.Background(), sink)
	ctx = durable.WithTurnScope(ctx, durable.NewTurnScope(inv))
	done := make(chan error, 1)
	go func() {
		request := brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")
		_, err := broker.Execute(ctx, request)
		done <- err
	}()
	select {
	case <-sink.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("approval event never emitted")
	}
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("approval events = %d, want 1", len(events))
	}
	decision, err := json.Marshal(ApprovalDecision{Approved: approve})
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	if err := backend.ResolveApproval(context.Background(), durable.ResolveApprovalRequest{
		ApprovalID: events[0].ApprovalID,
		Payload:    decision,
	}); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	return backend, sink, &calls, func() {
		select {
		case err := <-done:
			if approve && err != nil {
				t.Errorf("Execute after approval: %v", err)
			}
			if !approve && err == nil {
				t.Error("expected a rejection error")
			}
		case <-time.After(10 * time.Second):
			t.Error("Execute never returned after resolution")
		}
	}
}

func TestBrokerDurableParkApprovesOnce(t *testing.T) {
	backend, sink, calls, finish := parkBroker(t, true)
	finish()
	if *calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", *calls)
	}
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("approval events = %d, want exactly 1 (no duplicate emit)", len(events))
	}
	decision, err := json.Marshal(ApprovalDecision{Approved: true})
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	if err := backend.ResolveApproval(context.Background(), durable.ResolveApprovalRequest{
		ApprovalID: events[0].ApprovalID,
		Payload:    decision,
	}); err == nil {
		t.Fatal("expected a double-consume error")
	}
}

func TestBrokerDurableParkRejects(t *testing.T) {
	_, _, _, finish := parkBroker(t, false)
	finish()
}

func TestBrokerDurableParkReplaysDeterministically(t *testing.T) {
	policy := DefaultPolicy()
	readRule := policy.Rules["read_file"]
	readRule.RequiresApproval = true
	policy.Rules["read_file"] = readRule
	var calls int
	broker, err := New(&Options{
		Policy: policy,
		Adapters: map[string]Adapter{
			"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: []byte(`{"content":"x"}`)}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	backend := durable.NewFake()
	invCh := make(chan durable.Invocation, 1)
	backend.RegisterHandler("driver", func(_ context.Context, inv durable.Invocation) error {
		invCh <- inv
		select {}
	})
	if _, err := backend.Start(context.Background(), durable.StartRequest{Handler: "driver", Key: "turn-1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var inv durable.Invocation
	select {
	case inv = <-invCh:
	case <-time.After(5 * time.Second):
		t.Fatal("driver invocation never started")
	}
	sink := &recordingSink{ready: make(chan struct{})}
	runOnce := func() error {
		ctx := WithApprovalSink(context.Background(), sink)
		ctx = durable.WithTurnScope(ctx, durable.NewTurnScope(inv))
		returned := make(chan error, 1)
		go func() {
			_, err := broker.Execute(ctx, brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
			returned <- err
		}()
		return <-returned
	}
	firstDone := make(chan error, 1)
	go func() {
		ctx := WithApprovalSink(context.Background(), sink)
		ctx = durable.WithTurnScope(ctx, durable.NewTurnScope(inv))
		_, err := broker.Execute(ctx, brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
		firstDone <- err
	}()
	select {
	case <-sink.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("approval event never emitted")
	}
	firstEvents := sink.snapshot()
	if len(firstEvents) != 1 {
		t.Fatalf("first approval events = %d, want 1", len(firstEvents))
	}
	decision, err := json.Marshal(ApprovalDecision{Approved: true})
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	if err := backend.ResolveApproval(context.Background(), durable.ResolveApprovalRequest{
		ApprovalID: firstEvents[0].ApprovalID,
		Payload:    decision,
	}); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first Execute never returned")
	}
	if calls != 1 {
		t.Fatalf("adapter calls after first run = %d, want 1", calls)
	}
	if err := runOnce(); err != nil {
		t.Fatalf("replayed Execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("adapter calls after replay = %d, want 1 (single consumption)", calls)
	}
	replayed := sink.snapshot()
	if len(replayed) != 2 {
		t.Fatalf("approval events after replay = %d, want 2 (original plus identical replay emit)", len(replayed))
	}
	if replayed[0].ApprovalID != replayed[1].ApprovalID {
		t.Fatalf("replayed signal %q differs from original %q", replayed[1].ApprovalID, replayed[0].ApprovalID)
	}
	if !replayed[0].ExpiresAt.Equal(replayed[1].ExpiresAt) {
		t.Fatalf("replayed expiry %v differs from original %v", replayed[1].ExpiresAt, replayed[0].ExpiresAt)
	}
	if err := backend.ResolveApproval(context.Background(), durable.ResolveApprovalRequest{
		ApprovalID: firstEvents[0].ApprovalID,
		Payload:    decision,
	}); err == nil {
		t.Fatal("expected a double-consume error after replay")
	}
}
