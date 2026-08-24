package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ToolObservation is the metadata-only record of one completed tool call.
// Raw arguments, output, paths, URLs, and secrets never belong here.
type ToolObservation struct {
	Name          string
	Status        string
	PolicyOutcome string
	Approval      string
	Executor      string
	Duration      time.Duration
	OutputBytes   int64
}

// ToolRecorder turns tool observations into spans and bounded-label
// metrics. It is decoupled from any runtime: the broker reports after the
// fact, so spans carry the observed start/end timestamps.
type ToolRecorder struct {
	tracer   trace.Tracer
	calls    metric.Int64Counter
	duration metric.Float64Histogram
}

func NewToolRecorder(tp trace.TracerProvider, mp metric.MeterProvider) (*ToolRecorder, error) {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	calls, err := meter.Int64Counter(MetricToolCallsTotal, metric.WithDescription("completed tool calls by name, status, and output bucket"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create tool calls counter: %w", err)
	}
	duration, err := meter.Float64Histogram(MetricToolCallDuration, metric.WithUnit("s"), metric.WithDescription("tool call duration in seconds"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create tool duration histogram: %w", err)
	}
	return &ToolRecorder{tracer: tp.Tracer(ScopeName), calls: calls, duration: duration}, nil
}

func (r *ToolRecorder) Record(ctx context.Context, observation *ToolObservation) {
	end := time.Now()
	start := end.Add(-observation.Duration)
	labels := metric.WithAttributes(
		attribute.String(AttrToolName, observation.Name),
		attribute.String(AttrToolStatus, observation.Status),
		attribute.String(AttrToolPolicyOutcome, observation.PolicyOutcome),
		attribute.String(AttrToolApproval, observation.Approval),
		attribute.String(AttrToolExecutor, observation.Executor),
		attribute.String(AttrToolOutputBucket, OutputByteBucket(observation.OutputBytes)),
	)
	_, span := r.tracer.Start(ctx, SpanTool,
		trace.WithTimestamp(start),
		trace.WithAttributes(
			attribute.String(AttrToolName, observation.Name),
			attribute.String(AttrToolStatus, observation.Status),
			attribute.String(AttrToolPolicyOutcome, observation.PolicyOutcome),
			attribute.String(AttrToolApproval, observation.Approval),
			attribute.String(AttrToolExecutor, observation.Executor),
			attribute.Int64(AttrToolOutputBytes, observation.OutputBytes),
			attribute.String(AttrSemconvVersion, SemconvVersion),
		),
	)
	if observation.Status == "ok" {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, "")
	}
	span.End(trace.WithTimestamp(end))
	r.calls.Add(ctx, 1, labels)
	r.duration.Record(ctx, observation.Duration.Seconds(), labels)
}

var byteBucketBounds = []int64{
	1 << 10,
	4 << 10,
	16 << 10,
	64 << 10,
	256 << 10,
	1 << 20,
	4 << 20,
}

// OutputByteBucket maps a byte count to a fixed, bounded label so output
// size is observable without a high-cardinality dimension: "0" for empty
// output, then the smallest upper bound that fits.
func OutputByteBucket(bytes int64) string {
	if bytes <= 0 {
		return "0"
	}
	for _, bound := range byteBucketBounds {
		if bytes <= bound {
			return byteBucketLabel(bound)
		}
	}
	return "gt4m"
}

func byteBucketLabel(bound int64) string {
	switch {
	case bound >= 1<<20:
		return fmt.Sprintf("%dm", bound/(1<<20))
	default:
		return fmt.Sprintf("%dk", bound/(1<<10))
	}
}
