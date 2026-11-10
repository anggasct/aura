package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const MetricPrincipalAdmissionsTotal = "principal.admissions.total"

const (
	AttrPrincipalClass   = "principal.class"
	AttrPrincipalChannel = "principal.channel"
)

type PrincipalRecorder struct {
	admissions metric.Int64Counter
}

func NewPrincipalRecorder(mp metric.MeterProvider) (*PrincipalRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	admissions, err := mp.Meter(ScopeName).Int64Counter(MetricPrincipalAdmissionsTotal,
		metric.WithDescription("admitted channel turns by principal class and channel"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create principal admissions counter: %w", err)
	}
	return &PrincipalRecorder{admissions: admissions}, nil
}

func (r *PrincipalRecorder) Admitted(ctx context.Context, class, channel string) {
	if r == nil {
		return
	}
	r.admissions.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrPrincipalClass, class),
		attribute.String(AttrPrincipalChannel, channel),
	))
}
