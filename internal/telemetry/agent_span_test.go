package telemetry

import (
	"context"
	"testing"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTurnSpanCarriesAgentID(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	fr := &fakeRuntime{events: []store.RuntimeEvent{{Kind: runtime.EventKindTurnCompleted}}}
	inst, err := InstrumentRuntime(fr, tp, nil, nil)
	if err != nil {
		t.Fatalf("InstrumentRuntime: %v", err)
	}

	req := sampleTurnRequest()
	req.AgentID = "engineer"
	for _, err := range inst.Run(context.Background(), req) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	var found bool
	for _, span := range exporter.GetSpans() {
		if span.Name != SpanTurn {
			continue
		}
		for _, attr := range span.Attributes {
			if attr.Key == AttrAgentID {
				found = true
				if attr.Value.AsString() != "engineer" {
					t.Fatalf("agent.id = %q, want engineer", attr.Value.AsString())
				}
			}
		}
	}
	if !found {
		t.Fatal("turn span lacks the agent.id attribute")
	}
}
