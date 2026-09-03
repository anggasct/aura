package telemetry

import (
	"context"
	"iter"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

func TestSemconvVersionPinned(t *testing.T) {
	if SemconvVersion != "1.30.0" {
		t.Errorf("SemconvVersion = %q, want pinned 1.30.0; upgrading requires golden attribute/cardinality review", SemconvVersion)
	}
}

func TestSpanNamesPinned(t *testing.T) {
	spans := map[string]string{
		"SpanTurn":  SpanTurn,
		"SpanModel": SpanModel,
		"SpanTool":  SpanTool,
	}
	want := map[string]string{
		"SpanTurn":  "turn",
		"SpanModel": "model",
		"SpanTool":  "tool",
	}
	for name, got := range spans {
		if got != want[name] {
			t.Errorf("%s = %q, want %q; renaming is a breaking change for dashboards", name, got, want[name])
		}
	}
}

func TestMetricNamesPinned(t *testing.T) {
	metrics := map[string]string{
		"MetricTurnsTotal":       MetricTurnsTotal,
		"MetricTurnDuration":     MetricTurnDuration,
		"MetricModelDuration":    MetricModelDuration,
		"MetricToolCallsTotal":   MetricToolCallsTotal,
		"MetricToolCallDuration": MetricToolCallDuration,
	}
	want := map[string]string{
		"MetricTurnsTotal":       "runtime.turns.total",
		"MetricTurnDuration":     "runtime.turn.duration",
		"MetricModelDuration":    "gen_ai.client.operation.duration",
		"MetricToolCallsTotal":   "tools.calls.total",
		"MetricToolCallDuration": "tools.call.duration",
	}
	for name, got := range metrics {
		if got != want[name] {
			t.Errorf("%s = %q, want %q; renaming is a breaking change for dashboards", name, got, want[name])
		}
	}
}

func TestMetricUnitsPinned(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	reader := newTestManualReader()
	mp := newTestMeterProvider(reader)
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, mp, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	ctx := context.Background()
	for _, err := range inst.Run(ctx, sampleTurnRequest()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	_, mspan, mstart := inst.StartModelSpan(ctx, ModelSpanParams{System: "openai", RequestModel: "gpt-4o", Operation: "chat"})
	inst.EndModelSpan(ctx, mspan, mstart, "openai", "chat", "gpt-4o", 10, 5, nil)

	rm := collectMetrics(t, reader)
	units := map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			units[m.Name] = m.Unit
		}
	}
	wantUnits := map[string]string{
		MetricTurnDuration:  "s",
		MetricModelDuration: "s",
	}
	for name, wantUnit := range wantUnits {
		got, ok := units[name]
		if !ok {
			t.Errorf("metric %q not found in collected metrics", name)
			continue
		}
		if got != wantUnit {
			t.Errorf("metric %q unit = %q, want %q", name, got, wantUnit)
		}
	}
}

func TestTurnSpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanTurn)
	want := []string{
		AttrSessionID,
		AttrTurnID,
		AttrOrigin,
		AttrTerminalKind,
		AttrAgentID,
		AttrSemconvVersion,
	}
	assertAttrSet(t, "turn span", allowed, want)
}

func TestModelSpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanModel)
	want := []string{
		AttrGenAISystem,
		AttrGenAIRequestModel,
		AttrGenAIResponseModel,
		AttrGenAIOperationName,
		AttrGenAIUsageInputCount,
		AttrGenAIUsageOutputCount,
	}
	assertAttrSet(t, "model span", allowed, want)
}

func TestToolSpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanTool)
	want := []string{
		AttrToolName,
		AttrToolStatus,
		AttrToolPolicyOutcome,
		AttrToolApproval,
		AttrToolExecutor,
		AttrToolOutputBytes,
		AttrSemconvVersion,
	}
	assertAttrSet(t, "tool span", allowed, want)
}

func TestMetricLabelsBounded(t *testing.T) {
	cases := []struct {
		metric string
		want   []string
	}{
		{MetricTurnsTotal, []string{AttrOrigin, AttrTerminalKind}},
		{MetricTurnDuration, []string{AttrOrigin, AttrTerminalKind}},
		{MetricModelDuration, []string{AttrGenAISystem, AttrGenAIOperationName}},
		{MetricToolCallsTotal, []string{AttrToolName, AttrToolStatus, AttrToolPolicyOutcome, AttrToolApproval, AttrToolExecutor, AttrToolOutputBucket}},
		{MetricToolCallDuration, []string{AttrToolName, AttrToolStatus, AttrToolPolicyOutcome, AttrToolApproval, AttrToolExecutor, AttrToolOutputBucket}},
	}
	for _, tc := range cases {
		got := AllowedMetricLabels(tc.metric)
		if len(got) != len(tc.want) {
			t.Errorf("%s labels = %v, want %v", tc.metric, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s label[%d] = %q, want %q", tc.metric, i, got[i], tc.want[i])
			}
		}
	}
}

func TestMetricLabelsExcludeHighCardinality(t *testing.T) {
	highCardinality := []string{AttrSessionID, AttrTurnID}
	for metric, labels := range map[string][]string{
		MetricTurnsTotal:       AllowedMetricLabels(MetricTurnsTotal),
		MetricTurnDuration:     AllowedMetricLabels(MetricTurnDuration),
		MetricModelDuration:    AllowedMetricLabels(MetricModelDuration),
		MetricToolCallsTotal:   AllowedMetricLabels(MetricToolCallsTotal),
		MetricToolCallDuration: AllowedMetricLabels(MetricToolCallDuration),
	} {
		for _, label := range labels {
			for _, hc := range highCardinality {
				if label == hc {
					t.Errorf("%s has high-cardinality label %q; session/turn IDs belong in traces, not metrics", metric, hc)
				}
			}
		}
	}
}

func TestUnknownSpanHasNoAllowedAttrs(t *testing.T) {
	if got := AllowedSpanAttrs("nonexistent"); got != nil {
		t.Errorf("AllowedSpanAttrs(nonexistent) = %v, want nil", got)
	}
}

func TestSpanParentageUnderTurn(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inner := &contextCapturingRuntime{inner: fr, ch: make(chan context.Context, 1)}
	inst, err := InstrumentRuntime(inner, tp, nil, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}
	for _, err := range inst.Run(context.Background(), sampleTurnRequest()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	turnCtx := <-inner.ch
	if turnCtx == nil {
		t.Fatal("turn context was not captured")
	}

	_, mspan, mstart := inst.StartModelSpan(turnCtx, ModelSpanParams{System: "openai", RequestModel: "gpt-4o", Operation: "chat"})
	inst.EndModelSpan(turnCtx, mspan, mstart, "openai", "chat", "gpt-4o", 10, 5, nil)

	_, tspan := inst.StartToolSpan(turnCtx, ToolSpanParams{Name: "search"})
	inst.EndToolSpan(tspan, "completed", nil)

	spans := exporter.GetSpans()
	if len(spans) < 3 {
		t.Fatalf("spans = %d, want >= 3 (turn + model + tool)", len(spans))
	}

	spanByName := make(map[string]tracetest.SpanStub, len(spans))
	for _, s := range spans {
		spanByName[s.Name] = s
	}

	turnSpan, ok := spanByName[SpanTurn]
	if !ok {
		t.Fatalf("no span named %q", SpanTurn)
	}

	for _, childName := range []string{SpanModel, SpanTool} {
		child, ok := spanByName[childName]
		if !ok {
			t.Errorf("no span named %q", childName)
			continue
		}
		if child.Parent.SpanID() != turnSpan.SpanContext.SpanID() {
			t.Errorf("%s parent = %s, want turn span %s", childName, child.Parent.SpanID(), turnSpan.SpanContext.SpanID())
		}
		if child.Parent.TraceID() != turnSpan.SpanContext.TraceID() {
			t.Errorf("%s trace = %s, want turn trace %s", childName, child.Parent.TraceID(), turnSpan.SpanContext.TraceID())
		}
	}
}

type contextCapturingRuntime struct {
	inner *fakeRuntime
	ch    chan context.Context
}

func (c *contextCapturingRuntime) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	c.ch <- ctx
	return c.inner.Run(ctx, req)
}

func newTestManualReader() *sdkmetric.ManualReader {
	return sdkmetric.NewManualReader()
}

func newTestMeterProvider(reader *sdkmetric.ManualReader) *sdkmetric.MeterProvider {
	return sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func assertAttrSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s allowed attrs = %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s attr[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
