package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type MCPObservation struct {
	Server    string
	Transport string
	Tool      string
	Count     int
	Outcome   string
	Code      string
	SizeBytes int64
	Duration  time.Duration
}

type MCPRecorder struct {
	calls    metric.Int64Counter
	duration metric.Float64Histogram
	size     metric.Float64Histogram
}

func NewMCPRecorder(mp metric.MeterProvider) (*MCPRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &MCPRecorder{}
	if recorder.calls, err = meter.Int64Counter(MetricMCPCallsTotal,
		metric.WithDescription("mcp operations by server, transport, tool, outcome, and code")); err != nil {
		return nil, fmt.Errorf("telemetry: create mcp calls counter: %w", err)
	}
	if recorder.duration, err = meter.Float64Histogram(MetricMCPCallDuration,
		metric.WithUnit("s"), metric.WithDescription("mcp operation duration in seconds by server and outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create mcp call duration histogram: %w", err)
	}
	if recorder.size, err = meter.Float64Histogram(MetricMCPResponseSize,
		metric.WithUnit("By"), metric.WithDescription("mcp response size in bytes by server and outcome")); err != nil {
		return nil, fmt.Errorf("telemetry: create mcp response size histogram: %w", err)
	}
	return recorder, nil
}

func (r *MCPRecorder) Record(ctx context.Context, observation *MCPObservation) {
	if r == nil || observation == nil {
		return
	}
	full := metric.WithAttributes(
		attribute.String(AttrMCPServer, observation.Server),
		attribute.String(AttrMCPTransport, observation.Transport),
		attribute.String(AttrMCPTool, observation.Tool),
		attribute.String(AttrMCPOutcome, observation.Outcome),
		attribute.String(AttrMCPCode, observation.Code),
	)
	brief := metric.WithAttributes(
		attribute.String(AttrMCPServer, observation.Server),
		attribute.String(AttrMCPOutcome, observation.Outcome),
	)
	r.calls.Add(ctx, 1, full)
	r.duration.Record(ctx, observation.Duration.Seconds(), brief)
	r.size.Record(ctx, float64(observation.SizeBytes), brief)
}
