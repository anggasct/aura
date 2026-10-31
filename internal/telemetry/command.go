package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	MetricCommandInvocationsTotal = "command.invocations.total"
	SpanCommandExecute            = "command.execute"
)

const (
	AttrCommandName    = "command.name"
	AttrCommandChannel = "command.channel"
	AttrCommandResult  = "command.result"
)

type CommandRecorder struct {
	tracer      trace.Tracer
	invocations metric.Int64Counter
}

func NewCommandRecorder(tp trace.TracerProvider, mp metric.MeterProvider) (*CommandRecorder, error) {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	invocations, err := mp.Meter(ScopeName).Int64Counter(MetricCommandInvocationsTotal,
		metric.WithDescription("chat command invocations by command, channel, principal class, and result"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create command invocations counter: %w", err)
	}
	return &CommandRecorder{tracer: tp.Tracer(ScopeName), invocations: invocations}, nil
}

func (r *CommandRecorder) Start(ctx context.Context, name, channel, class string) (spanCtx context.Context, finish func(result string)) {
	if r == nil {
		return ctx, func(string) {}
	}
	base := []attribute.KeyValue{
		attribute.String(AttrCommandName, name),
		attribute.String(AttrCommandChannel, channel),
		attribute.String(AttrPrincipalClass, class),
	}
	spanCtx, span := r.tracer.Start(ctx, SpanCommandExecute, trace.WithAttributes(base...))
	return spanCtx, func(result string) {
		attrs := append(append([]attribute.KeyValue(nil), base...), attribute.String(AttrCommandResult, result))
		span.SetAttributes(attribute.String(AttrCommandResult, result))
		if result == "failed" {
			span.SetStatus(codes.Error, "")
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
		r.invocations.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}
