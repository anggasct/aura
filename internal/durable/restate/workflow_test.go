package restate

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/anggasct/aura/internal/durable"
)

type recordingStateWriter struct {
	states  []durable.RunState
	details []string
}

func (r *recordingStateWriter) setState(state durable.RunState, detail string) {
	r.states = append(r.states, state)
	r.details = append(r.details, detail)
}

func (r *recordingStateWriter) key() string { return "test-key" }

func TestFailRegisteredRunCancelledJournalsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := &recordingStateWriter{}
	out, err := failRegisteredRun(ctx, slog.New(slog.DiscardHandler), writer, errors.New("boom"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if out.State != string(durable.RunCancelled) || out.Detail != "boom" {
		t.Fatalf("output = %+v, want cancelled/boom", out)
	}
	if len(writer.states) != 1 || writer.states[0] != durable.RunCancelled {
		t.Fatalf("journaled states = %v, want [cancelled]", writer.states)
	}
	if len(writer.details) != 1 || writer.details[0] != "boom" {
		t.Fatalf("journaled details = %v, want [boom]", writer.details)
	}
}

func TestFailRegisteredRunFailureJournalsFailed(t *testing.T) {
	writer := &recordingStateWriter{}
	out, err := failRegisteredRun(context.Background(), slog.New(slog.DiscardHandler), writer, errors.New("boom"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out.State != string(durable.RunFailed) || out.Detail != "boom" {
		t.Fatalf("output = %+v, want failed/boom", out)
	}
	if len(writer.states) != 1 || writer.states[0] != durable.RunFailed {
		t.Fatalf("journaled states = %v, want [failed]", writer.states)
	}
}

func TestAdapterCancelServesCancelledStatus(t *testing.T) {
	stub := newStubIngress(t)
	adapter := newTestAdapter(t, stub)
	ctx := t.Context()

	stub.status["run-1"] = statusResponse{State: "running"}
	ref, err := adapter.Start(ctx, durable.StartRequest{Handler: "greet", Key: "run-1", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stub.status["run-1"] = statusResponse{State: "cancelled", Detail: "boom"}
	if err := adapter.Cancel(ctx, ref); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	status, err := adapter.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != durable.RunCancelled || status.Detail != "boom" {
		t.Fatalf("status = %+v, want cancelled/boom", status)
	}
}
