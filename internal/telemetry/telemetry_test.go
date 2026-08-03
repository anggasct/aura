package telemetry

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type fakeRuntime struct {
	events []store.RuntimeEvent
	err    error
}

func (f *fakeRuntime) Run(_ context.Context, _ *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		if f.err != nil {
			yield(store.RuntimeEvent{}, f.err)
			return
		}
		for i := range f.events {
			if !yield(f.events[i], nil) {
				return
			}
		}
	}
}

func newTestProviders(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader, *Instrument) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	fr := &fakeRuntime{events: []store.RuntimeEvent{
		{Kind: runtime.EventKindTurnAccepted},
		{Kind: runtime.EventKindTurnCompleted},
	}}
	inst, err := InstrumentRuntime(fr, tp, mp, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}
	return exporter, reader, inst
}

func sampleTurnRequest() *runtime.TurnRequest {
	return &runtime.TurnRequest{TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", Origin: runtime.OriginTerminal}
}

func TestInstrumentEmitsOneTraceWithoutContent(t *testing.T) {
	exporter, _, inst := newTestProviders(t)

	var got []store.RuntimeEvent
	for ev, err := range inst.Run(context.Background(), sampleTurnRequest()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("streamed events = %d, want 2 (passthrough)", len(got))
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want exactly one turn trace", len(spans))
	}
	span := spans[0]
	if span.Name != SpanTurn {
		t.Errorf("span name = %q, want %q", span.Name, SpanTurn)
	}

	var sawTerminal bool
	for _, attr := range span.Attributes {
		if isContentLeak(string(attr.Key)) {
			t.Errorf("content-bearing attribute leaked into telemetry: %s", attr.Key)
		}
		if attr.Key == AttrTerminalKind && attr.Value.AsString() == runtime.EventKindTurnCompleted {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Errorf("turn span missing terminal kind attribute; attrs=%v", span.Attributes)
	}
}

func TestInstrumentRecordsBoundedMetrics(t *testing.T) {
	_, reader, inst := newTestProviders(t)
	for _, err := range inst.Run(context.Background(), sampleTurnRequest()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := attribute.NewSet(attribute.String(AttrOrigin, "terminal"), attribute.String(AttrTerminalKind, runtime.EventKindTurnCompleted))
	if !hasMetricWithAttrs(rm, MetricTurnsTotal, want) {
		t.Errorf("missing %s datapoint with origin/terminal_kind labels", MetricTurnsTotal)
	}
	if !hasMetricWithAttrs(rm, MetricTurnDuration, want) {
		t.Errorf("missing %s datapoint with origin/terminal_kind labels", MetricTurnDuration)
	}
}

func TestInstrumentErrorPathMarksFailed(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	fr := &fakeRuntime{err: context.Canceled}
	inst, err := InstrumentRuntime(fr, tp, mp, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	var sawErr bool
	for _, err := range inst.Run(context.Background(), sampleTurnRequest()) {
		if err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the wrapped error to pass through")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", spans[0].Status.Code)
	}
}

func TestInstrumentNilProvidersFallBackToNoOp(t *testing.T) {
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, nil, nil, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime with nil providers: %v", err)
	}
	var n int
	for _, err := range inst.Run(context.Background(), sampleTurnRequest()) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("streamed events = %d, want 1 (passthrough)", n)
	}
}

func TestRedactDropsContentKeepsMetadata(t *testing.T) {
	in := map[string]any{
		AttrSessionID:    "session-1",
		AttrOrigin:       "terminal",
		"prompt":         "secret instructions",
		"tool_arguments": `{"rm":"-rf"}`,
		"Memory":         "user fact",
		"api_key":        "hunter2",
	}
	out := Redact(in)
	for _, safe := range []string{AttrSessionID, AttrOrigin} {
		if _, ok := out[safe]; !ok {
			t.Errorf("safe key %q was dropped", safe)
		}
	}
	for _, dropped := range []string{"prompt", "tool_arguments", "Memory", "api_key"} {
		if _, ok := out[dropped]; ok {
			t.Errorf("content key %q was not redacted", dropped)
		}
	}
}

func TestModelSpanHierarchy(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
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

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1 (turn only, no model span without explicit call)", len(spans))
	}
}

func TestModelSpanExplicit(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, mp, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	ctx := context.Background()
	mctx, mspan, mstart := inst.StartModelSpan(ctx, ModelSpanParams{
		System:       "openai",
		RequestModel: "gpt-4o",
		Operation:    "chat",
	})
	_ = mctx
	inst.EndModelSpan(ctx, mspan, mstart, "openai", "chat", "gpt-4o-2024-08-06", 100, 50, nil)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != SpanModel {
		t.Errorf("span name = %q, want %q", span.Name, SpanModel)
	}
	if span.Status.Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", span.Status.Code)
	}
	attrMap := spanAttrMap(&span)
	if attrMap[AttrGenAISystem] != "openai" {
		t.Errorf("gen_ai.system = %q, want openai", attrMap[AttrGenAISystem])
	}
	if attrMap[AttrGenAIResponseModel] != "gpt-4o-2024-08-06" {
		t.Errorf("gen_ai.response.model = %q, want gpt-4o-2024-08-06", attrMap[AttrGenAIResponseModel])
	}
	for _, attr := range span.Attributes {
		if isContentLeak(string(attr.Key)) {
			t.Errorf("content-bearing attribute leaked: %s", attr.Key)
		}
	}
}

func TestToolSpanExplicit(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, mp, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	ctx := context.Background()
	_, tspan := inst.StartToolSpan(ctx, ToolSpanParams{Name: "web_search"})
	inst.EndToolSpan(tspan, "completed", nil)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != SpanTool {
		t.Errorf("span name = %q, want %q", span.Name, SpanTool)
	}
	attrMap := spanAttrMap(&span)
	if attrMap[AttrToolName] != "web_search" {
		t.Errorf("tool.name = %q, want web_search", attrMap[AttrToolName])
	}
	if attrMap[AttrToolStatus] != "completed" {
		t.Errorf("tool.status = %q, want completed", attrMap[AttrToolStatus])
	}
}

func TestDroppedCounterShared(t *testing.T) {
	dropped := &atomic.Int64{}
	dropped.Store(42)
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, nil, nil, dropped)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}
	if inst.dropped.Load() != 42 {
		t.Errorf("dropped = %d, want 42 (shared counter)", inst.dropped.Load())
	}
}

func spanAttrMap(span *tracetest.SpanStub) map[string]string {
	m := make(map[string]string, len(span.Attributes))
	for _, attr := range span.Attributes {
		m[string(attr.Key)] = attr.Value.AsString()
	}
	return m
}

func isContentLeak(key string) bool {
	if safeKeys[key] {
		return false
	}
	return isContentKey(key)
}

func hasMetricWithAttrs(rm metricdata.ResourceMetrics, name string, want attribute.Set) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					if dp.Attributes.Equals(&want) {
						return true
					}
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					if dp.Attributes.Equals(&want) {
						return true
					}
				}
			}
		}
	}
	return false
}
