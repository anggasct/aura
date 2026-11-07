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
	CodePhaseBuild = "build"
	CodePhaseRun   = "run"
)

var codeByteBounds = []float64{1 << 10, 16 << 10, 256 << 10, 1 << 20, 4 << 20, 16 << 20, 64 << 20}

type CodeObservation struct {
	Runtime       string
	ResultClass   string
	Phase         string
	Compiled      bool
	BuildDuration time.Duration
	RunDuration   time.Duration
	BuildCPU      time.Duration
	RunCPU        time.Duration
	OutputBytes   int64
	ArtifactBytes int64
}

type CodeRecorder struct {
	runs      metric.Int64Counter
	duration  metric.Float64Histogram
	cpu       metric.Float64Histogram
	output    metric.Int64Histogram
	artifacts metric.Int64Histogram
}

func NewCodeRecorder(mp metric.MeterProvider) (*CodeRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &CodeRecorder{}
	if recorder.runs, err = meter.Int64Counter(MetricCodeRunsTotal,
		metric.WithDescription("code_execute runs by runtime, result class, and the phase they ended in")); err != nil {
		return nil, fmt.Errorf("telemetry: create code runs counter: %w", err)
	}
	if recorder.duration, err = meter.Float64Histogram(MetricCodeRunDuration,
		metric.WithUnit("s"), metric.WithDescription("code_execute phase duration in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create code duration histogram: %w", err)
	}
	if recorder.cpu, err = meter.Float64Histogram(MetricCodeRunCPU,
		metric.WithUnit("s"), metric.WithDescription("code_execute phase CPU time in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create code cpu histogram: %w", err)
	}
	if recorder.output, err = meter.Int64Histogram(MetricCodeRunOutput,
		metric.WithUnit("By"), metric.WithExplicitBucketBoundaries(codeByteBounds...),
		metric.WithDescription("code_execute captured stdout and stderr bytes")); err != nil {
		return nil, fmt.Errorf("telemetry: create code output histogram: %w", err)
	}
	if recorder.artifacts, err = meter.Int64Histogram(MetricCodeRunArtifacts,
		metric.WithUnit("By"), metric.WithExplicitBucketBoundaries(codeByteBounds...),
		metric.WithDescription("code_execute imported artifact bytes")); err != nil {
		return nil, fmt.Errorf("telemetry: create code artifact histogram: %w", err)
	}
	return recorder, nil
}

func (r *CodeRecorder) Record(ctx context.Context, observation *CodeObservation) {
	if r == nil || observation == nil {
		return
	}
	runtimeOnly := metric.WithAttributes(attribute.String(AttrCodeRuntime, observation.Runtime))
	r.runs.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrCodeRuntime, observation.Runtime),
		attribute.String(AttrCodeResultClass, observation.ResultClass),
		attribute.String(AttrCodePhase, observation.Phase),
	))
	r.output.Record(ctx, observation.OutputBytes, runtimeOnly)
	if observation.Compiled {
		r.recordPhase(ctx, observation, CodePhaseBuild, observation.BuildDuration, observation.BuildCPU)
	}
	if observation.Phase == CodePhaseRun {
		r.recordPhase(ctx, observation, CodePhaseRun, observation.RunDuration, observation.RunCPU)
		r.artifacts.Record(ctx, observation.ArtifactBytes, runtimeOnly)
	}
}

func (r *CodeRecorder) recordPhase(ctx context.Context, observation *CodeObservation, phase string, duration, cpu time.Duration) {
	labels := metric.WithAttributes(
		attribute.String(AttrCodeRuntime, observation.Runtime),
		attribute.String(AttrCodeResultClass, observation.ResultClass),
		attribute.String(AttrCodePhase, phase),
	)
	r.duration.Record(ctx, duration.Seconds(), labels)
	r.cpu.Record(ctx, cpu.Seconds(), labels)
}
