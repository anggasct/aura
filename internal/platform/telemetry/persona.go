package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const MetricPersonaTurnsTotal = "persona.turns.total"

type PersonaRecorder struct {
	turns metric.Int64Counter
}

func NewPersonaRecorder(mp metric.MeterProvider) (*PersonaRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	turns, err := mp.Meter(ScopeName).Int64Counter(MetricPersonaTurnsTotal,
		metric.WithDescription("executed turns by persona"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: create persona turns counter: %w", err)
	}
	return &PersonaRecorder{turns: turns}, nil
}

func (r *PersonaRecorder) Turn(ctx context.Context, agentID string) {
	if r == nil || agentID == "" {
		return
	}
	r.turns.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrAgentID, agentID)))
}
