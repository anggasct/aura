package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/anggasct/aura/internal/config"
)

const (
	ExporterNone     = "none"
	ExporterStdout   = "stdout"
	ExporterOTLPGRPC = "otlp_grpc"
	ExporterOTLPHTTP = "otlp_http"
)

type Pipeline struct {
	tp       *sdktrace.TracerProvider
	mp       *sdkmetric.MeterProvider
	dropped  atomic.Int64
	logger   *slog.Logger
	shutdown atomic.Bool
}

func NewPipeline(cfg config.Telemetry, logger *slog.Logger) (*Pipeline, error) {
	if logger == nil {
		logger = slog.Default()
	}
	exporter := cfg.Exporter
	if exporter == "" {
		exporter = ExporterNone
	}
	if !validExporter(exporter) {
		return nil, &Error{Code: ErrorCodeExporterInvalid, Detail: fmt.Sprintf("unsupported exporter %q", exporter)}
	}

	res := resource.Default()

	p := &Pipeline{logger: logger}

	if exporter == ExporterNone {
		p.tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res))
		p.mp = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res))
		return p, nil
	}

	spanExporter, metricExporter, err := buildExporters(cfg, exporter)
	if err != nil {
		return nil, err
	}

	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = 2048
	}
	exportTimeout := time.Duration(cfg.ExportTimeout)
	if exportTimeout <= 0 {
		exportTimeout = 5 * time.Second
	}

	counting := &countingExporter{inner: spanExporter, dropped: &p.dropped, logger: logger}

	batchOpts := []sdktrace.BatchSpanProcessorOption{
		sdktrace.WithMaxQueueSize(queueSize),
		sdktrace.WithMaxExportBatchSize(min(queueSize, 512)),
		sdktrace.WithExportTimeout(exportTimeout),
	}

	sampleRatio := cfg.SampleRatio
	if sampleRatio <= 0 {
		sampleRatio = 0.10
	}
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))

	p.tp = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(counting, batchOpts...)),
	)

	periodicOpts := []sdkmetric.PeriodicReaderOption{
		sdkmetric.WithTimeout(exportTimeout),
		sdkmetric.WithInterval(30 * time.Second),
	}
	p.mp = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, periodicOpts...)),
	)

	return p, nil
}

func (p *Pipeline) TracerProvider() *sdktrace.TracerProvider { return p.tp }
func (p *Pipeline) MeterProvider() *sdkmetric.MeterProvider  { return p.mp }
func (p *Pipeline) Dropped() int64                           { return p.dropped.Load() }

func (p *Pipeline) Shutdown(ctx context.Context) error {
	if !p.shutdown.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	if p.tp != nil {
		if err := p.tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: shutdown tracer provider: %w", err))
		}
	}
	if p.mp != nil {
		if err := p.mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: shutdown meter provider: %w", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetry: shutdown: %w", errs[0])
	}
	return nil
}

func validExporter(e string) bool {
	switch e {
	case ExporterNone, ExporterStdout, ExporterOTLPGRPC, ExporterOTLPHTTP:
		return true
	}
	return false
}

func buildExporters(cfg config.Telemetry, exporter string) (sdktrace.SpanExporter, sdkmetric.Exporter, error) {
	ctx := context.Background()
	switch exporter {
	case ExporterStdout:
		se, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: create stdout trace exporter: %w", err)
		}
		me, err := stdoutmetric.New()
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: create stdout metric exporter: %w", err)
		}
		return se, me, nil
	case ExporterOTLPGRPC:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		mopts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
		if !isTLSEndpoint(cfg.Endpoint) {
			opts = append(opts, otlptracegrpc.WithInsecure())
			mopts = append(mopts, otlpmetricgrpc.WithInsecure())
		}
		se, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: create otlp grpc trace exporter: %w", err)
		}
		me, err := otlpmetricgrpc.New(ctx, mopts...)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: create otlp grpc metric exporter: %w", err)
		}
		return se, me, nil
	case ExporterOTLPHTTP:
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		mopts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.Endpoint)}
		if !isTLSEndpoint(cfg.Endpoint) {
			opts = append(opts, otlptracehttp.WithInsecure())
			mopts = append(mopts, otlpmetrichttp.WithInsecure())
		}
		se, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: create otlp http trace exporter: %w", err)
		}
		me, err := otlpmetrichttp.New(ctx, mopts...)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: create otlp http metric exporter: %w", err)
		}
		return se, me, nil
	default:
		return nil, nil, &Error{Code: ErrorCodeExporterInvalid, Detail: fmt.Sprintf("unsupported exporter %q", exporter)}
	}
}

func isTLSEndpoint(endpoint string) bool {
	return endpoint != "" && endpoint[:1] != ":" && !hasPortOnly(endpoint)
}

func hasPortOnly(endpoint string) bool {
	for i := range len(endpoint) {
		if endpoint[i] == ':' {
			return false
		}
		if endpoint[i] < '0' || endpoint[i] > '9' {
			return false
		}
	}
	return endpoint != ""
}

type countingExporter struct {
	inner   sdktrace.SpanExporter
	dropped *atomic.Int64
	logger  *slog.Logger
}

func (e *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.inner.ExportSpans(ctx, spans); err != nil {
		e.dropped.Add(int64(len(spans)))
		if e.logger != nil {
			e.logger.WarnContext(ctx, "telemetry export failed", "dropped_spans", len(spans), "error", err)
		}
		return err
	}
	return nil
}

func (e *countingExporter) Shutdown(ctx context.Context) error {
	return e.inner.Shutdown(ctx)
}
