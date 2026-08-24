package toolbroker

import (
	"context"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func observingBroker(t *testing.T, observations *[]Observation, observerCtxs *[]context.Context, adapters map[string]Adapter) *Broker {
	t.Helper()
	broker, err := New(&Options{
		Adapters: adapters,
		Observer: func(ctx context.Context, observation Observation) {
			*observations = append(*observations, observation)
			*observerCtxs = append(*observerCtxs, ctx)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return broker
}

func passthroughAdapters() map[string]Adapter {
	return map[string]Adapter{
		"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: []byte(`{"content":"x"}`)}, nil
		},
		"write_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: []byte(`{"written":true}`)}, nil
		},
		"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: []byte(`{"exit_code":0}`)}, nil
		},
	}
}
func TestBrokerObservationCarriesPolicyApprovalExecutorMetadata(t *testing.T) {
	var observations []Observation
	var observerCtxs []context.Context
	policy := DefaultPolicy()
	readRule := policy.Rules["read_file"]
	readRule.RequiresApproval = true
	policy.Rules["read_file"] = readRule
	broker, err := New(&Options{
		Policy: policy,
		Adapters: map[string]Adapter{
			"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: []byte(`{"content":"x"}`)}, nil
			},
			"list_dir@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: []byte(`{"entries":[]}`)}, nil
			},
		},
		Observer: func(ctx context.Context, observation Observation) {
			observations = append(observations, observation)
			observerCtxs = append(observerCtxs, ctx)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// allow + auto approval + direct executor
	if _, err := broker.Execute(context.Background(), brokerRequest("list_dir", `{"path":"."}`, "workspace-read")); err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	// require_approval + missing grant
	if _, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")); classOf(err) != ResultApprovalRequired {
		t.Fatalf("read_file without approval = %v", err)
	}
	// require_approval + attached grant
	withApproval := brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")
	withApproval.RequestID = "request-2"
	withApproval.IdempotencyKey = "idempotency-2"
	grant, err := broker.Grant(context.Background(), withApproval, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	withApproval.Approval = &grant
	if _, err := broker.Execute(context.Background(), withApproval); err != nil {
		t.Fatalf("read_file with approval: %v", err)
	}
	// pre-evaluation failure keeps the bounded not_evaluated outcome
	if _, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt","extra":1}`, "workspace-read")); classOf(err) != ResultInvalidArgument {
		t.Fatalf("invalid arguments = %v", err)
	}

	want := []struct {
		class         ResultClass
		policyOutcome string
		approval      string
		executor      string
	}{
		{ResultOK, PolicyOutcomeAllow, ApprovalAuto, ExecutorDirect},
		{ResultApprovalRequired, PolicyOutcomeRequireApproval, ApprovalMissing, ""},
		{ResultOK, PolicyOutcomeRequireApproval, ApprovalAttached, ExecutorDirect},
		{ResultInvalidArgument, PolicyOutcomeNotEvaluated, "", ""},
	}
	if len(observations) != len(want) {
		t.Fatalf("observations = %d, want %d", len(observations), len(want))
	}
	for i, tc := range want {
		got := observations[i]
		if got.Class != tc.class || got.PolicyOutcome != tc.policyOutcome || got.Approval != tc.approval || got.Executor != tc.executor {
			t.Errorf("observation[%d] = class %q policy %q approval %q executor %q, want %q %q %q %q",
				i, got.Class, got.PolicyOutcome, got.Approval, got.Executor, tc.class, tc.policyOutcome, tc.approval, tc.executor)
		}
	}
}

func TestBrokerObservationCarriesExecutingContext(t *testing.T) {
	var observations []Observation
	var observerCtxs []context.Context
	broker := observingBroker(t, &observations, &observerCtxs, passthroughAdapters())

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, parent := tp.Tracer("test").Start(context.Background(), "turn")
	defer parent.End()

	if _, err := broker.Execute(ctx, brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if len(observerCtxs) != 1 {
		t.Fatalf("observer calls = %d, want 1", len(observerCtxs))
	}
	observed := trace.SpanFromContext(observerCtxs[0])
	if !observed.SpanContext().IsValid() {
		t.Fatal("observer context carries no valid span; tool spans cannot attach to the active turn")
	}
	if observed.SpanContext().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("observer span = %v, want the parent turn span %v", observed.SpanContext().SpanID(), parent.SpanContext().SpanID())
	}
}
