package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	MetricOperationInvocationsTotal  = "operations.invocations.total"
	MetricOperationInvocationLatency = "operations.invocation.duration"
)

const (
	AttrOperationID      = "aura.operation.id"
	AttrOperationSurface = "aura.operation.surface"
	AttrOperationRisk    = "aura.operation.risk"
	AttrOperationOutcome = "aura.operation.outcome"
)

type OperationObservation struct {
	ID       string
	Surface  string
	Risk     string
	Outcome  string
	Duration time.Duration
}

type OperationRecorder struct {
	invocations metric.Int64Counter
	duration    metric.Float64Histogram
}

func NewOperationRecorder(mp metric.MeterProvider) (*OperationRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &OperationRecorder{}
	if recorder.invocations, err = meter.Int64Counter(MetricOperationInvocationsTotal,
		metric.WithDescription("operation invocations by operation, surface, risk, and outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create operation invocations counter: %w", err)
	}
	if recorder.duration, err = meter.Float64Histogram(MetricOperationInvocationLatency,
		metric.WithUnit("s"), metric.WithDescription("operation invocation duration in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create operation duration histogram: %w", err)
	}
	return recorder, nil
}

func (r *OperationRecorder) Record(ctx context.Context, observation *OperationObservation) {
	if r == nil || observation == nil {
		return
	}
	options := metric.WithAttributeSet(attribute.NewSet(
		attribute.String(AttrOperationID, observation.ID),
		attribute.String(AttrOperationSurface, observation.Surface),
		attribute.String(AttrOperationRisk, observation.Risk),
		attribute.String(AttrOperationOutcome, observation.Outcome),
	))
	r.invocations.Add(ctx, 1, options)
	r.duration.Record(ctx, observation.Duration.Seconds(), options)
}
