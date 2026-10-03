package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type VisionObservation struct {
	Operation string
	Result    string
	MIME      string
	Images    int
	Version   string
	Protocol  string
	Duration  time.Duration
}

type VisionRecorder struct {
	operations metric.Int64Counter
	duration   metric.Float64Histogram
}

func NewVisionRecorder(mp metric.MeterProvider) (*VisionRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &VisionRecorder{}
	if recorder.operations, err = meter.Int64Counter(MetricVisionOperationsTotal,
		metric.WithDescription("vision operations by operation, result, mime, version, and protocol")); err != nil {
		return nil, fmt.Errorf("telemetry: create vision operations counter: %w", err)
	}
	if recorder.duration, err = meter.Float64Histogram(MetricVisionOperationDuration,
		metric.WithUnit("s"), metric.WithDescription("vision operation duration in seconds by operation and result")); err != nil {
		return nil, fmt.Errorf("telemetry: create vision operation duration histogram: %w", err)
	}
	return recorder, nil
}

func (r *VisionRecorder) Record(ctx context.Context, observation *VisionObservation) {
	if r == nil || observation == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String(AttrVisionOperation, observation.Operation),
		attribute.String(AttrVisionResult, observation.Result),
		attribute.String(AttrVisionMIME, observation.MIME),
		attribute.String(AttrVisionVersion, observation.Version),
		attribute.String(AttrVisionProtocol, observation.Protocol),
	)
	r.operations.Add(ctx, 1, attrs)
	r.duration.Record(ctx, observation.Duration.Seconds(), attrs)
}
