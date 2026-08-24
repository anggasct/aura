package telemetry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func newToolTestTracer(t *testing.T) (*tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exporter, tp
}

func TestToolRecorderRecordsBoundedLabels(t *testing.T) {
	exporter, tp := newToolTestTracer(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	recorder, err := NewToolRecorder(tp, mp)
	if err != nil {
		t.Fatalf("NewToolRecorder: %v", err)
	}

	recorder.Record(context.Background(), &ToolObservation{Name: "read_file", Status: "ok", Duration: 150 * time.Millisecond, OutputBytes: 300})
	recorder.Record(context.Background(), &ToolObservation{Name: "exec", Status: "policy_denied", Duration: 5 * time.Millisecond, OutputBytes: 0})

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	first := spans[0]
	if first.Name != SpanTool {
		t.Errorf("span name = %q, want %q", first.Name, SpanTool)
	}
	attrs := map[string]string{}
	for _, kv := range first.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs[AttrToolName] != "read_file" || attrs[AttrToolStatus] != "ok" {
		t.Errorf("span attrs = %v", attrs)
	}
	if elapsed := first.EndTime.Sub(first.StartTime); elapsed < 100*time.Millisecond {
		t.Errorf("span duration = %v, want the observed ~150ms", elapsed)
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counters := map[string]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricToolCallsTotal {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				counters[attributeValue(dp, AttrToolName)+"/"+attributeValue(dp, AttrToolStatus)+"/"+attributeValue(dp, AttrToolOutputBucket)] += dp.Value
			}
		}
	}
	if counters["read_file/ok/1k"] != 1 {
		t.Errorf("read_file counter = %v, want 1 at bucket 1k", counters)
	}
	if counters["exec/policy_denied/0"] != 1 {
		t.Errorf("exec counter = %v, want 1 at bucket 0", counters)
	}
}

func TestToolRecorderRecordsPolicyApprovalExecutorDimensions(t *testing.T) {
	exporter, tp := newToolTestTracer(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	recorder, err := NewToolRecorder(tp, mp)
	if err != nil {
		t.Fatalf("NewToolRecorder: %v", err)
	}

	recorder.Record(context.Background(), &ToolObservation{
		Name: "read_file", Status: "ok", PolicyOutcome: "allow", Approval: "auto", Executor: "direct",
		Duration: 10 * time.Millisecond, OutputBytes: 100,
	})
	recorder.Record(context.Background(), &ToolObservation{
		Name: "write_file", Status: "approval_required", PolicyOutcome: "require_approval", Approval: "missing", Executor: "",
		Duration: 2 * time.Millisecond, OutputBytes: 0,
	})

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs[AttrToolPolicyOutcome] != "allow" || attrs[AttrToolApproval] != "auto" || attrs[AttrToolExecutor] != "direct" {
		t.Errorf("span policy/approval/executor = %q/%q/%q, want allow/auto/direct",
			attrs[AttrToolPolicyOutcome], attrs[AttrToolApproval], attrs[AttrToolExecutor])
	}
	denied := map[string]string{}
	for _, kv := range spans[1].Attributes {
		denied[string(kv.Key)] = kv.Value.String()
	}
	if denied[AttrToolPolicyOutcome] != "require_approval" || denied[AttrToolApproval] != "missing" {
		t.Errorf("denied span policy/approval = %q/%q, want require_approval/missing",
			denied[AttrToolPolicyOutcome], denied[AttrToolApproval])
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := false
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != MetricToolCallsTotal {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if attributeValue(dp, AttrToolName) == "read_file" &&
					attributeValue(dp, AttrToolPolicyOutcome) == "allow" &&
					attributeValue(dp, AttrToolApproval) == "auto" &&
					attributeValue(dp, AttrToolExecutor) == "direct" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("tool calls counter is missing the allow/auto/direct dimension combination")
	}
}

// The broker observer wires the per-execution context into the recorder, so
// a tool span started inside a turn must be its child.
func TestToolSpanIsChildOfTurnSpanThroughBroker(t *testing.T) {
	exporter, tp := newToolTestTracer(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	recorder, err := NewToolRecorder(tp, mp)
	if err != nil {
		t.Fatalf("NewToolRecorder: %v", err)
	}
	observer := toolbroker.Observer(func(ctx context.Context, observation toolbroker.Observation) {
		recorder.Record(ctx, &ToolObservation{
			Name:          observation.ToolName + "@" + observation.ToolVersion,
			Status:        string(observation.Class),
			PolicyOutcome: observation.PolicyOutcome,
			Approval:      observation.Approval,
			Executor:      observation.Executor,
			Duration:      observation.Duration,
			OutputBytes:   observation.OutputBytes,
		})
	})
	broker, err := toolbroker.New(&toolbroker.Options{
		Adapters: map[string]toolbroker.Adapter{
			"list_dir@v1": func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				return toolbroker.ToolResult{Output: []byte(`{"entries":[]}`)}, nil
			},
		},
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("New broker: %v", err)
	}

	ctx, turn := tp.Tracer(ScopeName).Start(context.Background(), SpanTurn)
	request := &toolbroker.ToolRequest{
		RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
		ToolName: "list_dir", ToolVersion: "v1", Arguments: json.RawMessage(`{"path":"."}`),
		Capabilities: []string{"workspace-read"}, Trust: approval.TrustOwnerInput, IdempotencyKey: "idempotency-1",
	}
	if _, err := broker.Execute(ctx, request); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	turn.End()

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2 (turn + tool)", len(spans))
	}
	toolSpan := spans[0]
	if toolSpan.Name != SpanTool {
		t.Fatalf("first span = %q, want the tool span", toolSpan.Name)
	}
	if toolSpan.Parent.SpanID() != turn.SpanContext().SpanID() {
		t.Fatalf("tool span parent = %v, want the turn span %v", toolSpan.Parent.SpanID(), turn.SpanContext().SpanID())
	}
}

func TestOutputByteBucketIsBounded(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		-1:        "0",
		1:         "1k",
		100:       "1k",
		1024:      "1k",
		4096:      "4k",
		1 << 20:   "1m",
		4 << 20:   "4m",
		5 << 20:   "gt4m",
		1 << 30:   "gt4m",
		1<<31 - 1: "gt4m",
	}
	for bytes, want := range cases {
		if got := OutputByteBucket(bytes); got != want {
			t.Errorf("OutputByteBucket(%d) = %q, want %q", bytes, got, want)
		}
	}
	if len(byteBucketBounds)+1 > 10 {
		t.Errorf("bucket cardinality = %d, keep it small", len(byteBucketBounds)+1)
	}
}

// The broker observation adapter must forward metadata only; this pins the
// mapping the CLI composition relies on.
func TestBrokerObservationMapsToToolObservation(t *testing.T) {
	exporter, tp := newToolTestTracer(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	recorder, err := NewToolRecorder(tp, mp)
	if err != nil {
		t.Fatalf("NewToolRecorder: %v", err)
	}
	observer := toolbroker.Observer(func(ctx context.Context, observation toolbroker.Observation) {
		recorder.Record(ctx, &ToolObservation{
			Name:          observation.ToolName + "@" + observation.ToolVersion,
			Status:        string(observation.Class),
			PolicyOutcome: observation.PolicyOutcome,
			Approval:      observation.Approval,
			Executor:      observation.Executor,
			Duration:      observation.Duration,
			OutputBytes:   observation.OutputBytes,
		})
	})
	observer(context.Background(), toolbroker.Observation{ToolName: "read_file", ToolVersion: "v1", Class: toolbroker.ResultOK, Duration: time.Millisecond, OutputBytes: 10})
	if len(exporter.GetSpans()) != 1 {
		t.Fatalf("spans = %d, want 1", len(exporter.GetSpans()))
	}
	attrs := map[string]string{}
	for _, kv := range exporter.GetSpans()[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs[AttrToolName] != "read_file@v1" || attrs[AttrToolStatus] != "ok" {
		t.Errorf("attrs = %v", attrs)
	}
}

func attributeValue(dp metricdata.DataPoint[int64], key attribute.Key) string {
	value, ok := dp.Attributes.Value(key)
	if !ok {
		return ""
	}
	return value.AsString()
}
