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
	MetricOperationSearchTotal       = "operations.search.total"
	MetricOperationLoadedTools       = "operations.loaded.tools"
)

const (
	SearchOutcomeMatched     = "matched"
	SearchOutcomeEmpty       = "empty"
	SearchOutcomeSessionFull = "session_full"
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
	searches    metric.Int64Counter
	loaded      metric.Int64Histogram
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
	if recorder.searches, err = meter.Int64Counter(MetricOperationSearchTotal,
		metric.WithDescription("operation discovery searches by outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create operation search counter: %w", err)
	}
	if recorder.loaded, err = meter.Int64Histogram(MetricOperationLoadedTools,
		metric.WithDescription("operation tools loaded in a session after a search")); err != nil {
		return nil, fmt.Errorf("telemetry: create operation loaded tools histogram: %w", err)
	}
	return recorder, nil
}

func (r *OperationRecorder) RecordSearch(ctx context.Context, outcome string, loaded int) {
	if r == nil {
		return
	}
	r.searches.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attribute.String(AttrOperationOutcome, outcome))))
	r.loaded.Record(ctx, int64(loaded))
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
