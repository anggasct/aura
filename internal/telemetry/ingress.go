package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	MetricIngressDroppedTotal = "channel.ingress.dropped.total"
	AttrIngressDropReason     = "ingress.drop.reason"
)

type IngressRecorder struct {
	dropped metric.Int64Counter
}

func NewIngressRecorder(mp metric.MeterProvider) (*IngressRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	dropped, err := mp.Meter(ScopeName).Int64Counter(MetricIngressDroppedTotal,
		metric.WithDescription("channel messages dropped before admission by reason"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create ingress dropped counter: %w", err)
	}
	return &IngressRecorder{dropped: dropped}, nil
}

func (r *IngressRecorder) Dropped(ctx context.Context, channel, reason string) {
	if r == nil {
		return
	}
	r.dropped.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrPrincipalChannel, channel),
		attribute.String(AttrIngressDropReason, reason),
	))
}
