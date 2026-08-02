// Package telemetry defines Aura's observability contract: a fixed signal
// vocabulary, a default-deny redaction baseline, and a turn instrument that
// wraps the runtime so every turn exposes one correlated trace and bounded
// metrics. Content is never emitted; only low-cardinality metadata is.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

// ScopeName identifies Aura's instrumentation scope.
const ScopeName = "aura"

// Span names. One trace per turn; child spans (model, tool, policy, storage)
// nest under the turn span as their features land.
const (
	SpanTurn = "turn"
)

// Metric instrument names. Labels are bounded to low-cardinality metadata;
// session, turn, and event IDs are never metric labels.
const (
	MetricTurnsTotal   = "runtime.turns.total"
	MetricTurnDuration = "runtime.turn.duration"
)

// Attribute keys. Span attributes may carry IDs; metric attributes use only
// the low-cardinality subset (origin, terminal kind).
const (
	AttrSessionID    = "session.id"
	AttrTurnID       = "turn.id"
	AttrOrigin       = "turn.origin"
	AttrTerminalKind = "turn.terminal_kind"
)

// contentPatterns are key fragments that mark an attribute as content-bearing.
// Redact drops any attribute whose lowercased key contains one of these, so
// prompts, tool arguments/results, memory, secrets, and attachments never
// reach telemetry. Safe metadata keys (session.id, turn.origin, ...) contain
// none of these fragments.
var contentPatterns = []string{
	"prompt", "content", "message", "text", "argument", "result",
	"memory", "attachment", "secret", "token", "password", "credential",
	"body", "payload", "input", "output", "key",
}

// Redact returns only the attributes whose keys are not content-bearing. It is
// default-deny: any key matching a content pattern is dropped.
func Redact(attrs map[string]any) map[string]any {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if isContentKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

func isContentKey(key string) bool {
	lower := strings.ToLower(key)
	for _, p := range contentPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// Instrument decorates an AgentRuntime, emitting one turn span and bounded
// turn metrics per Run. Nil providers fall back to the global no-op providers,
// so an unconfigured instrument adds no export and no content exposure.
type Instrument struct {
	inner        runtime.AgentRuntime
	tracer       trace.Tracer
	turnsTotal   metric.Int64Counter
	turnDuration metric.Float64Histogram
}

// InstrumentRuntime wraps inner with turn tracing and metrics. Nil providers
// select the global (no-op by default) providers.
func InstrumentRuntime(inner runtime.AgentRuntime, tp trace.TracerProvider, mp metric.MeterProvider) (*Instrument, error) {
	if inner == nil {
		return nil, errors.New("telemetry: runtime must not be nil")
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
	return &Instrument{inner: inner, tracer: tracer, turnsTotal: turnsTotal, turnDuration: turnDuration}, nil
}

// Run starts a turn span, delegates to the wrapped runtime, and records the
// terminal outcome. The span carries redacted metadata only; the stream is
// passed through unchanged.
func (i *Instrument) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		spanAttrs := Redact(map[string]any{
			AttrSessionID: req.SessionID,
			AttrTurnID:    req.TurnID,
			AttrOrigin:    string(req.Origin),
		})
		ctx, span := i.tracer.Start(ctx, SpanTurn, trace.WithAttributes(toKeyValues(spanAttrs)...))
		defer span.End()
		start := time.Now()

		terminal := runtime.EventKindTurnFailed
		for ev, err := range i.inner.Run(ctx, req) {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "")
				i.record(ctx, start, req, runtime.EventKindTurnFailed)
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
		i.record(ctx, start, req, terminal)
	}
}

func (i *Instrument) record(ctx context.Context, start time.Time, req *runtime.TurnRequest, terminal string) {
	labels := metric.WithAttributes(attribute.String(AttrOrigin, string(req.Origin)), attribute.String(AttrTerminalKind, terminal))
	i.turnsTotal.Add(ctx, 1, labels)
	i.turnDuration.Record(ctx, time.Since(start).Seconds(), labels)
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
