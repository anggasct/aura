package telemetry

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

const ScopeName = "aura"

var contentPatterns = []string{
	"prompt", "content", "message", "text", "argument", "result",
	"memory", "attachment", "secret", "token", "password", "credential",
	"body", "payload", "input", "output", "key",
	"filename", "header", "profile", "url",
}

var safeKeys = map[string]bool{
	AttrGenAIUsageInputCount:   true,
	AttrGenAIUsageOutputCount:  true,
	AttrModelRoute:             true,
	AttrModelCandidate:         true,
	AttrModelFallbackAttempt:   true,
	AttrModelCircuitState:      true,
	AttrModelCircuitTransition: true,
	AttrModelNormalizedResult:  true,
}

func Redact(attrs map[string]any) map[string]any {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if safeKeys[k] {
			out[k] = v
			continue
		}
		if isContentKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

func isContentKey(k string) bool {
	lower := strings.ToLower(k)
	for _, p := range contentPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

type Instrument struct {
	inner          runtime.AgentRuntime
	tracer         trace.Tracer
	turnsTotal     metric.Int64Counter
	turnDuration   metric.Float64Histogram
	modelDuration  metric.Float64Histogram
	dropped        *atomic.Int64
	defaultAgentID string
}

// InstrumentOption adjusts optional instrumentation behavior.
type InstrumentOption func(*Instrument)

// WithDefaultAgentID names the definition that serves turns whose request
// does not target one explicitly; the turn span then carries that id as
// agent.id alongside explicitly targeted turns.
func WithDefaultAgentID(id string) InstrumentOption {
	return func(i *Instrument) { i.defaultAgentID = id }
}

func InstrumentRuntime(inner runtime.AgentRuntime, tp trace.TracerProvider, mp metric.MeterProvider, dropped *atomic.Int64, opts ...InstrumentOption) (*Instrument, error) {
	if inner == nil {
		return nil, &Error{Code: ErrorCodeInstrumentFailed, Detail: "runtime must not be nil"}
	}
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	tracer := tp.Tracer(ScopeName)
	meter := mp.Meter(ScopeName)
	turnsTotal, err := meter.Int64Counter(MetricTurnsTotal, metric.WithDescription("completed turns by origin and terminal kind"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create turns counter: %w", err)
	}
	turnDuration, err := meter.Float64Histogram(MetricTurnDuration, metric.WithUnit("s"), metric.WithDescription("turn duration in seconds"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create duration histogram: %w", err)
	}
	modelDuration, err := meter.Float64Histogram(MetricModelDuration, metric.WithUnit("s"), metric.WithDescription("model operation duration in seconds"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create model duration histogram: %w", err)
	}
	if dropped == nil {
		dropped = &atomic.Int64{}
	}
	instrument := &Instrument{
		inner:         inner,
		tracer:        tracer,
		turnsTotal:    turnsTotal,
		turnDuration:  turnDuration,
		modelDuration: modelDuration,
		dropped:       dropped,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(instrument)
		}
	}
	return instrument, nil
}

func (i *Instrument) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		spanAttrs := Redact(map[string]any{
			AttrSessionID:      req.SessionID,
			AttrTurnID:         req.TurnID,
			AttrOrigin:         string(req.Origin),
			AttrSemconvVersion: SemconvVersion,
		})
		agentID := req.AgentID
		if agentID == "" {
			agentID = i.defaultAgentID
		}
		if agentID != "" {
			spanAttrs[AttrAgentID] = agentID
		}
		ctx, span := i.tracer.Start(ctx, SpanTurn, trace.WithAttributes(toKeyValues(spanAttrs)...))
		defer span.End()
		start := time.Now()

		terminal := runtime.EventKindTurnFailed
		for ev, err := range i.inner.Run(ctx, req) {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "")
				i.recordTurn(ctx, start, req, runtime.EventKindTurnFailed)
				yield(ev, err)
				return
			}
			if isTerminalKind(ev.Kind) {
				terminal = ev.Kind
			}
			if !yield(ev, nil) {
				return
			}
		}

		if terminal == runtime.EventKindTurnCompleted {
			span.SetStatus(codes.Ok, "")
		} else {
			span.SetStatus(codes.Error, "")
		}
		span.SetAttributes(attribute.String(AttrTerminalKind, terminal))
		i.recordTurn(ctx, start, req, terminal)
	}
}

func (i *Instrument) recordTurn(ctx context.Context, start time.Time, req *runtime.TurnRequest, terminal string) {
	labels := metric.WithAttributes(attribute.String(AttrOrigin, string(req.Origin)), attribute.String(AttrTerminalKind, terminal))
	i.turnsTotal.Add(ctx, 1, labels)
	i.turnDuration.Record(ctx, time.Since(start).Seconds(), labels)
}

type ModelSpanParams struct {
	System       string
	RequestModel string
	Operation    string
}

func (i *Instrument) StartModelSpan(ctx context.Context, params ModelSpanParams) (context.Context, trace.Span, time.Time) {
	attrs := Redact(map[string]any{
		AttrGenAISystem:        params.System,
		AttrGenAIRequestModel:  params.RequestModel,
		AttrGenAIOperationName: params.Operation,
	})
	ctx, span := i.tracer.Start(ctx, SpanModel, trace.WithAttributes(toKeyValues(attrs)...))
	return ctx, span, time.Now()
}

func (i *Instrument) EndModelSpan(ctx context.Context, span trace.Span, start time.Time, system, operation, responseModel string, inputTokens, outputTokens int64, err error) {
	if responseModel != "" {
		span.SetAttributes(attribute.String(AttrGenAIResponseModel, responseModel))
	}
	span.SetAttributes(
		attribute.Int64(AttrGenAIUsageInputCount, inputTokens),
		attribute.Int64(AttrGenAIUsageOutputCount, outputTokens),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
	labels := metric.WithAttributes(
		attribute.String(AttrGenAISystem, system),
		attribute.String(AttrGenAIOperationName, operation),
	)
	i.modelDuration.Record(ctx, time.Since(start).Seconds(), labels)
}

type ToolSpanParams struct {
	Name string
}

func (i *Instrument) StartToolSpan(ctx context.Context, params ToolSpanParams) (context.Context, trace.Span) {
	attrs := Redact(map[string]any{
		AttrToolName: params.Name,
	})
	ctx, span := i.tracer.Start(ctx, SpanTool, trace.WithAttributes(toKeyValues(attrs)...))
	return ctx, span
}

func (i *Instrument) EndToolSpan(span trace.Span, status string, err error) {
	span.SetAttributes(attribute.String(AttrToolStatus, status))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func isTerminalKind(kind string) bool {
	switch kind {
	case runtime.EventKindTurnCompleted, runtime.EventKindTurnFailed, runtime.EventKindTurnCancelled:
		return true
	}
	return false
}

func toKeyValues(attrs map[string]any) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		out = append(out, attribute.String(k, fmt.Sprint(v)))
	}
	return out
}
