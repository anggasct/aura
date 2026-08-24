package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

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

	recorder.Record(context.Background(), ToolObservation{Name: "read_file", Status: "ok", Duration: 150 * time.Millisecond, OutputBytes: 300})
	recorder.Record(context.Background(), ToolObservation{Name: "exec", Status: "policy_denied", Duration: 5 * time.Millisecond, OutputBytes: 0})

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
	observer := toolbroker.Observer(func(observation toolbroker.Observation) {
		recorder.Record(context.Background(), ToolObservation{
			Name:        observation.ToolName + "@" + observation.ToolVersion,
			Status:      string(observation.Class),
			Duration:    observation.Duration,
			OutputBytes: observation.OutputBytes,
		})
	})
	observer(toolbroker.Observation{ToolName: "read_file", ToolVersion: "v1", Class: toolbroker.ResultOK, Duration: time.Millisecond, OutputBytes: 10})
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
