package telemetry

import (
	"context"
	"iter"
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
	inst, err := InstrumentRuntime(fr, tp, mp)
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
		if isContentKey(string(attr.Key)) {
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
	inst, err := InstrumentRuntime(fr, tp, mp)
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
