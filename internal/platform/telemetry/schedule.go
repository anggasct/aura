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
	MetricScheduleFiresTotal   = "schedule.fires.total"
	MetricScheduleFireLag      = "schedule.fire.lag"
	MetricScheduleSettlesTotal = "schedule.settles.total"
	MetricScheduleSettleAge    = "schedule.settle.age"
	MetricScheduleRetriesTotal = "schedule.retries.total"
)

const (
	AttrScheduleState  = "schedule.state"
	AttrScheduleResult = "schedule.result"
)

type ScheduleObservation struct {
	State  string
	Result string
	Lag    time.Duration
	Age    time.Duration
}

type ScheduleRecorder struct {
	fires   metric.Int64Counter
	lag     metric.Float64Histogram
	settles metric.Int64Counter
	age     metric.Float64Histogram
	retries metric.Int64Counter
}

func NewScheduleRecorder(mp metric.MeterProvider) (*ScheduleRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &ScheduleRecorder{}
	if recorder.fires, err = meter.Int64Counter(MetricScheduleFiresTotal,
		metric.WithDescription("scheduled fires recorded by state")); err != nil {
		return nil, fmt.Errorf("telemetry: create schedule fires counter: %w", err)
	}
	if recorder.lag, err = meter.Float64Histogram(MetricScheduleFireLag,
		metric.WithUnit("s"), metric.WithDescription("scheduled fire lateness in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create schedule lag histogram: %w", err)
	}
	if recorder.settles, err = meter.Int64Counter(MetricScheduleSettlesTotal,
		metric.WithDescription("settled occurrences by state")); err != nil {
		return nil, fmt.Errorf("telemetry: create schedule settles counter: %w", err)
	}
	if recorder.age, err = meter.Float64Histogram(MetricScheduleSettleAge,
		metric.WithUnit("s"), metric.WithDescription("occurrence age at settlement in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create schedule age histogram: %w", err)
	}
	if recorder.retries, err = meter.Int64Counter(MetricScheduleRetriesTotal,
		metric.WithDescription("overload retries")); err != nil {
		return nil, fmt.Errorf("telemetry: create schedule retries counter: %w", err)
	}
	return recorder, nil
}

func (r *ScheduleRecorder) Record(ctx context.Context, observation *ScheduleObservation) {
	if r == nil || observation == nil {
		return
	}
	opt := metric.WithAttributeSet(attribute.NewSet(
		attribute.String(AttrScheduleState, observation.State),
		attribute.String(AttrScheduleResult, observation.Result),
	))
	switch observation.Result {
	case "fired":
		r.fires.Add(ctx, 1, opt)
		r.lag.Record(ctx, observation.Lag.Seconds(), opt)
	case "settled":
		r.settles.Add(ctx, 1, opt)
		r.age.Record(ctx, observation.Age.Seconds(), opt)
	case "retried":
		r.retries.Add(ctx, 1, opt)
	}
}
