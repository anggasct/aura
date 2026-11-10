package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type ChildObservation struct {
	State      string
	Result     string
	TokensUsed int64
	CostMicros int64
}

type ChildActiveCounts struct {
	Queued  int64
	Running int64
}

type ChildRecorder struct {
	spawns       metric.Int64Counter
	settles      metric.Int64Counter
	budgetTokens metric.Float64Histogram
	budgetCost   metric.Float64Histogram
}

func NewChildRecorder(mp metric.MeterProvider, active func() ChildActiveCounts) (*ChildRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &ChildRecorder{}
	if recorder.spawns, err = meter.Int64Counter(MetricChildSpawnsTotal,
		metric.WithDescription("child agent spawn attempts by result")); err != nil {
		return nil, fmt.Errorf("telemetry: create child spawns counter: %w", err)
	}
	if recorder.settles, err = meter.Int64Counter(MetricChildSettlesTotal,
		metric.WithDescription("child agent terminal settlements by state and result")); err != nil {
		return nil, fmt.Errorf("telemetry: create child settles counter: %w", err)
	}
	if recorder.budgetTokens, err = meter.Float64Histogram(MetricChildBudgetTokens,
		metric.WithUnit("{token}"),
		metric.WithDescription("child agent tokens charged per settlement")); err != nil {
		return nil, fmt.Errorf("telemetry: create child budget tokens histogram: %w", err)
	}
	if recorder.budgetCost, err = meter.Float64Histogram(MetricChildBudgetCost,
		metric.WithUnit("{micro}"),
		metric.WithDescription("child agent cost micros charged per settlement")); err != nil {
		return nil, fmt.Errorf("telemetry: create child budget cost histogram: %w", err)
	}
	if active != nil {
		if _, err = meter.Int64ObservableGauge(MetricChildActive,
			metric.WithUnit("{child}"),
			metric.WithDescription("active child agent runs by state"),
			metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
				counts := active()
				observer.Observe(counts.Queued, metric.WithAttributes(attribute.String(AttrChildState, "queued")))
				observer.Observe(counts.Running, metric.WithAttributes(attribute.String(AttrChildState, "running")))
				return nil
			})); err != nil {
			return nil, fmt.Errorf("telemetry: create child active gauge: %w", err)
		}
	}
	return recorder, nil
}

func (r *ChildRecorder) RecordSpawn(ctx context.Context, result string) {
	if r == nil {
		return
	}
	r.spawns.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrChildResult, result)))
}

func (r *ChildRecorder) RecordSettle(ctx context.Context, observation *ChildObservation) {
	if r == nil || observation == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String(AttrChildState, observation.State),
		attribute.String(AttrChildResult, observation.Result),
	)
	r.settles.Add(ctx, 1, attrs)
	r.budgetTokens.Record(ctx, float64(observation.TokensUsed))
	r.budgetCost.Record(ctx, float64(observation.CostMicros))
}
