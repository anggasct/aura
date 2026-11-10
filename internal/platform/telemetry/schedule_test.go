package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestScheduleRecorderEmitsLifecycleMetrics(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewScheduleRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewScheduleRecorder(): %v", err)
	}
	ctx := context.Background()
	recorder.Record(ctx, &ScheduleObservation{State: "fired", Result: "fired", Lag: 5 * time.Second})
	recorder.Record(ctx, &ScheduleObservation{State: "completed", Result: "settled", Age: 30 * time.Second})
	recorder.Record(ctx, &ScheduleObservation{State: "failed", Result: "settled", Age: time.Minute})
	recorder.Record(ctx, &ScheduleObservation{Result: "retried"})
	names := map[string]bool{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			names[point.Name] = true
		}
	}
	for _, want := range []string{
		MetricScheduleFiresTotal,
		MetricScheduleFireLag,
		MetricScheduleSettlesTotal,
		MetricScheduleSettleAge,
		MetricScheduleRetriesTotal,
	} {
		if !names[want] {
			t.Errorf("metric %q missing", want)
		}
	}
}

func TestScheduleRecorderIgnoresNil(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewScheduleRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewScheduleRecorder(): %v", err)
	}
	recorder.Record(context.Background(), nil)
	var nilRecorder *ScheduleRecorder
	nilRecorder.Record(context.Background(), &ScheduleObservation{Result: "fired"})
	count := 0
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		count += len(scope.Metrics)
	}
	if count != 0 {
		t.Errorf("nil observations produced %d metrics", count)
	}
}

func TestScheduleMetricLabelsExcludeIdentity(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewScheduleRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewScheduleRecorder(): %v", err)
	}
	ctx := context.Background()
	recorder.Record(ctx, &ScheduleObservation{State: "fired", Result: "fired", Lag: time.Second})
	recorder.Record(ctx, &ScheduleObservation{State: "completed", Result: "settled", Age: time.Second})
	allowed := map[string]bool{AttrScheduleState: true, AttrScheduleResult: true}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
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
