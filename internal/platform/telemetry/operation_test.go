package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOperationMetricNamesPinned(t *testing.T) {
	if MetricOperationInvocationsTotal != "operations.invocations.total" {
		t.Errorf("MetricOperationInvocationsTotal = %q, want operations.invocations.total", MetricOperationInvocationsTotal)
	}
	if MetricOperationInvocationLatency != "operations.invocation.duration" {
		t.Errorf("MetricOperationInvocationLatency = %q, want operations.invocation.duration", MetricOperationInvocationLatency)
	}
}

func TestOperationRecorderCountsInvocationsWithFourAttributes(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewOperationRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewOperationRecorder(): %v", err)
	}
	recorder.Record(context.Background(), &OperationObservation{ID: "cron.list", Surface: "agent", Risk: "read", Outcome: "ok", Duration: 20 * time.Millisecond})

	want := attribute.NewSet(
		attribute.String(AttrOperationID, "cron.list"),
		attribute.String(AttrOperationSurface, "agent"),
		attribute.String(AttrOperationRisk, "read"),
		attribute.String(AttrOperationOutcome, "ok"),
	)
	found := map[string]bool{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			switch data := point.Data.(type) {
			case metricdata.Sum[int64]:
				if len(data.DataPoints) == 1 && data.DataPoints[0].Value == 1 && data.DataPoints[0].Attributes.Equals(&want) {
					found[point.Name] = true
				}
			case metricdata.Histogram[float64]:
				if len(data.DataPoints) == 1 && data.DataPoints[0].Count == 1 && data.DataPoints[0].Attributes.Equals(&want) {
					found[point.Name] = true
				}
			}
		}
	}
	for _, name := range []string{MetricOperationInvocationsTotal, MetricOperationInvocationLatency} {
		if !found[name] {
			t.Errorf("metric %q has no point with the four operation attributes", name)
		}
	}
}

func TestOperationRecorderIgnoresNil(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewOperationRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewOperationRecorder(): %v", err)
	}
	recorder.Record(context.Background(), nil)
	var nilRecorder *OperationRecorder
	nilRecorder.Record(context.Background(), &OperationObservation{Outcome: "ok"})
	if scopes := collectMetrics(t, reader).ScopeMetrics; len(scopes) != 0 {
		t.Errorf("nil observations produced %d metric scopes", len(scopes))
	}
}
