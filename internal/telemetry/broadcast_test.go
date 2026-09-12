package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func recordBroadcastSample(recorder *BroadcastRecorder) {
	ctx := context.Background()
	recorder.Record(ctx, &BroadcastObservation{Priority: "info", State: "held", Result: "submitted"})
	recorder.Record(ctx, &BroadcastObservation{Priority: "warning", State: "succeeded", Result: "delivered", Attempts: 1, Age: 30 * time.Second})
	recorder.Record(ctx, &BroadcastObservation{Priority: "warning", State: "unknown", Result: "unknown", Attempts: 3, Age: time.Minute})
	recorder.Record(ctx, &BroadcastObservation{Priority: "info", State: "failed", Result: "failed", Attempts: 3, Age: 2 * time.Minute})
	recorder.Record(ctx, &BroadcastObservation{Priority: "info", Result: "retried", Attempts: 1})
	recorder.Record(ctx, &BroadcastObservation{Priority: "warning", Result: "fallback"})
	recorder.Record(ctx, &BroadcastObservation{Priority: "info", State: "scheduled", Result: "digested", DigestCount: 4})
	recorder.Record(ctx, &BroadcastObservation{Priority: "info", Result: "rate_delayed", RateDelay: 5 * time.Second})
}

func broadcastMetricNames(rm metricdata.ResourceMetrics) map[string]bool {
	names := map[string]bool{}
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			names[point.Name] = true
		}
	}
	return names
}

func TestBroadcastRecorderEmitsLifecycleMetrics(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewBroadcastRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewBroadcastRecorder(): %v", err)
	}
	recordBroadcastSample(recorder)
	names := broadcastMetricNames(collectMetrics(t, reader))
	for _, want := range []string{
		MetricBroadcastDeliveriesTotal,
		MetricBroadcastDeliveryAge,
		MetricBroadcastRetriesTotal,
		MetricBroadcastFallbacksTotal,
		MetricBroadcastHoldsTotal,
		MetricBroadcastDigestsTotal,
		MetricBroadcastRateDelay,
	} {
		if !names[want] {
			t.Errorf("metric %q missing", want)
		}
	}
}

func TestBroadcastRecorderIgnoresNil(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewBroadcastRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewBroadcastRecorder(): %v", err)
	}
	recorder.Record(context.Background(), nil)
	var nilRecorder *BroadcastRecorder
	nilRecorder.Record(context.Background(), &BroadcastObservation{Result: "delivered"})
	if names := broadcastMetricNames(collectMetrics(t, reader)); len(names) != 0 {
		t.Errorf("nil observations produced metrics: %v", names)
	}
}

func TestBroadcastMetricLabelsExcludeIdentity(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewBroadcastRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewBroadcastRecorder(): %v", err)
	}
	recordBroadcastSample(recorder)
	rm := collectMetrics(t, reader)
	allowed := map[string]bool{
		AttrBroadcastPriority: true,
		AttrBroadcastState:    true,
		AttrBroadcastResult:   true,
		AttrBroadcastAttempt:  true,
	}
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			aggregate := func(attrs []attribute.KeyValue) {
				for _, attr := range attrs {
					if !allowed[string(attr.Key)] {
						t.Errorf("metric %q carries non-vocabulary label %q", point.Name, string(attr.Key))
					}
				}
			}
			switch data := point.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					aggregate(point.Attributes.ToSlice())
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					aggregate(point.Attributes.ToSlice())
				}
			}
		}
	}
}
