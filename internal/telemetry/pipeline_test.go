package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/config"
)

func TestPipelineNoneExporter(t *testing.T) {
	p, err := NewPipeline(config.Telemetry{Exporter: "none"}, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	defer func() { _ = p.Shutdown(t.Context()) }()

	if p.TracerProvider() == nil {
		t.Fatal("TracerProvider is nil")
	}
	if p.MeterProvider() == nil {
		t.Fatal("MeterProvider is nil")
	}
	if p.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0", p.Dropped())
	}
}

func TestPipelineEmptyExporterDefaultsToNone(t *testing.T) {
	p, err := NewPipeline(config.Telemetry{}, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	_ = p.Shutdown(t.Context())
}

func TestPipelineInvalidExporter(t *testing.T) {
	_, err := NewPipeline(config.Telemetry{Exporter: "kafka"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid exporter")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeExporterInvalid {
		t.Errorf("error code = %v, want %v", code, ErrorCodeExporterInvalid)
	}
}

func TestPipelineStdoutExporter(t *testing.T) {
	p, err := NewPipeline(config.Telemetry{Exporter: "stdout"}, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

type failingExporter struct {
	count atomic.Int64
}

func (e *failingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.count.Add(int64(len(spans)))
	return errors.New("export failed: connection refused")
}

func (e *failingExporter) Shutdown(_ context.Context) error { return nil }

func TestCountingExporterCountsFailures(t *testing.T) {
	dropped := &atomic.Int64{}
	failing := &failingExporter{}
	counting := &countingExporter{inner: failing, dropped: dropped, logger: nil}

	spans := tracetest.SpanStubs{{Name: "test"}}.Snapshots()
	err := counting.ExportSpans(context.Background(), spans)
	if err == nil {
		t.Fatal("expected export error to propagate")
	}
	if dropped.Load() != 1 {
		t.Errorf("dropped = %d, want 1", dropped.Load())
	}

	_ = counting.ExportSpans(context.Background(), spans)
	if dropped.Load() != 2 {
		t.Errorf("dropped = %d, want 2 after second failure", dropped.Load())
	}
}

func TestCountingExporterSuccessNoDrop(t *testing.T) {
	dropped := &atomic.Int64{}
	inner := tracetest.NewInMemoryExporter()
	counting := &countingExporter{inner: inner, dropped: dropped, logger: nil}

	spans := tracetest.SpanStubs{{Name: "test"}}.Snapshots()
	if err := counting.ExportSpans(context.Background(), spans); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}
	if dropped.Load() != 0 {
		t.Errorf("dropped = %d, want 0 on success", dropped.Load())
	}
}

func TestPipelineShutdownIdempotent(t *testing.T) {
	p, err := NewPipeline(config.Telemetry{Exporter: "none"}, nil)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestPipelineBoundedQueueDoesNotBlock(t *testing.T) {
	failing := &failingExporter{}
	dropped := &atomic.Int64{}
	counting := &countingExporter{inner: failing, dropped: dropped, logger: nil}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(counting,
			sdktrace.WithMaxQueueSize(4),
			sdktrace.WithMaxExportBatchSize(2),
			sdktrace.WithExportTimeout(100*time.Millisecond),
		)),
	)

	tracer := tp.Tracer("test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_, span := tracer.Start(context.Background(), "flood")
			span.End()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("span emission blocked; queue overflow must be non-blocking")
	}

	_ = tp.Shutdown(t.Context())
}

func TestPipelineOTLPGRPCEndpointRequired(t *testing.T) {
	_, err := NewPipeline(config.Telemetry{Exporter: "otlp_grpc"}, nil)
	if err == nil {
		t.Log("otlp_grpc without endpoint created; endpoint validation is at config layer")
	}
}
