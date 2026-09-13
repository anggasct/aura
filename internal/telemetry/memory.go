package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type MemoryObservation struct {
	Outcome     string
	Summarized  bool
	Documents   int
	Screened    int
	ModelSystem string
	Duration    time.Duration
}

type MemoryRecorder struct {
	recalls   metric.Int64Counter
	duration  metric.Float64Histogram
	documents metric.Float64Histogram
}

func NewMemoryRecorder(mp metric.MeterProvider) (*MemoryRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &MemoryRecorder{}
	if recorder.recalls, err = meter.Int64Counter(MetricMemoryRecallsTotal,
		metric.WithDescription("session memory recall attempts by outcome and model system")); err != nil {
		return nil, fmt.Errorf("telemetry: create memory recalls counter: %w", err)
	}
	if recorder.duration, err = meter.Float64Histogram(MetricMemoryRecallDuration,
		metric.WithUnit("s"), metric.WithDescription("session memory recall duration in seconds by outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create memory recall duration histogram: %w", err)
	}
	if recorder.documents, err = meter.Float64Histogram(MetricMemoryRecallDocuments,
		metric.WithUnit("{document}"), metric.WithDescription("session memory recall documents by outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create memory recall documents histogram: %w", err)
	}
	return recorder, nil
}

func (r *MemoryRecorder) Record(ctx context.Context, observation *MemoryObservation) {
	if r == nil || observation == nil {
		return
	}
	outcomeOpt := metric.WithAttributes(attribute.String(AttrRecallOutcome, observation.Outcome))
	systemOpt := metric.WithAttributes(
		attribute.String(AttrRecallOutcome, observation.Outcome),
		attribute.String(AttrRecallModelSystem, observation.ModelSystem),
	)
	r.recalls.Add(ctx, 1, systemOpt)
	r.duration.Record(ctx, observation.Duration.Seconds(), outcomeOpt)
	r.documents.Record(ctx, float64(observation.Documents), outcomeOpt)
}
