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
	MetricBroadcastDeliveriesTotal = "broadcast.deliveries.total"
	MetricBroadcastDeliveryAge     = "broadcast.delivery.age"
	MetricBroadcastRetriesTotal    = "broadcast.retries.total"
	MetricBroadcastFallbacksTotal  = "broadcast.fallbacks.total"
	MetricBroadcastHoldsTotal      = "broadcast.holds.total"
	MetricBroadcastDigestsTotal    = "broadcast.digests.total"
	MetricBroadcastRateDelay       = "broadcast.rate.delay"
)

const (
	AttrBroadcastPriority = "broadcast.priority"
	AttrBroadcastState    = "broadcast.state"
	AttrBroadcastResult   = "broadcast.result"
	AttrBroadcastAttempt  = "broadcast.attempt"
)

type BroadcastObservation struct {
	Priority    string
	State       string
	Result      string
	Attempts    int
	Age         time.Duration
	RateDelay   time.Duration
	DigestCount int
}

type BroadcastRecorder struct {
	deliveries metric.Int64Counter
	age        metric.Float64Histogram
	retries    metric.Int64Counter
	fallbacks  metric.Int64Counter
	holds      metric.Int64Counter
	digests    metric.Int64Counter
	rateDelay  metric.Float64Histogram
}

func NewBroadcastRecorder(mp metric.MeterProvider) (*BroadcastRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &BroadcastRecorder{}
	if recorder.deliveries, err = meter.Int64Counter(MetricBroadcastDeliveriesTotal,
		metric.WithDescription("settled broadcast deliveries by priority, state, and result")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast deliveries counter: %w", err)
	}
	if recorder.age, err = meter.Float64Histogram(MetricBroadcastDeliveryAge,
		metric.WithUnit("s"), metric.WithDescription("broadcast age at settlement in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast age histogram: %w", err)
	}
	if recorder.retries, err = meter.Int64Counter(MetricBroadcastRetriesTotal,
		metric.WithDescription("broadcast delivery retries by priority and attempt")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast retries counter: %w", err)
	}
	if recorder.fallbacks, err = meter.Int64Counter(MetricBroadcastFallbacksTotal,
		metric.WithDescription("broadcast fallback deliveries by priority and result")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast fallbacks counter: %w", err)
	}
	if recorder.holds, err = meter.Int64Counter(MetricBroadcastHoldsTotal,
		metric.WithDescription("broadcast notifications held for quiet hours by priority")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast holds counter: %w", err)
	}
	if recorder.digests, err = meter.Int64Counter(MetricBroadcastDigestsTotal,
		metric.WithDescription("broadcast digest parents created by priority")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast digests counter: %w", err)
	}
	if recorder.rateDelay, err = meter.Float64Histogram(MetricBroadcastRateDelay,
		metric.WithUnit("s"), metric.WithDescription("broadcast rate-limit delay in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create broadcast rate delay histogram: %w", err)
	}
	return recorder, nil
}

func broadcastAttributes(priority, state, result string) attribute.Set {
	return attribute.NewSet(
		attribute.String(AttrBroadcastPriority, priority),
		attribute.String(AttrBroadcastState, state),
		attribute.String(AttrBroadcastResult, result),
	)
}

func (r *BroadcastRecorder) Record(ctx context.Context, observation *BroadcastObservation) {
	if r == nil || observation == nil {
		return
	}
	base := broadcastAttributes(observation.Priority, observation.State, observation.Result)
	opt := metric.WithAttributeSet(base)
	switch observation.Result {
	case "submitted":
		if observation.State == "held" {
			r.holds.Add(ctx, 1, opt)
		}
	case "delivered", "unknown", "failed":
		r.deliveries.Add(ctx, 1, opt)
		r.age.Record(ctx, observation.Age.Seconds(), opt)
	case "retried":
		retryOpt := metric.WithAttributeSet(attribute.NewSet(
			attribute.String(AttrBroadcastPriority, observation.Priority),
			attribute.Int(AttrBroadcastAttempt, observation.Attempts),
		))
		r.retries.Add(ctx, 1, retryOpt)
	case "fallback":
		r.fallbacks.Add(ctx, 1, opt)
	case "digested":
		r.digests.Add(ctx, 1, opt)
	case "rate_delayed":
		r.rateDelay.Record(ctx, observation.RateDelay.Seconds(), opt)
	}
}
