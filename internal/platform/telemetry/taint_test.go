package telemetry

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestTaintTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(t.Context()) })

	rec, err := NewTaintRecorder(mp)
	if err != nil {
		t.Fatalf("NewTaintRecorder: %v", err)
	}
	rec.RecordMark(t.Context(), "tool_result")
	rec.RecordWorkspaceRecord(t.Context(), "write")
	rec.RecordWorkspaceRecord(t.Context(), "exec")
	rec.RecordWorkspaceRecord(t.Context(), "carried")
	rec.RecordWorkspaceRecord(t.Context(), "wide")
	rec.RecordConfirmation(t.Context(), "applied")
	rec.RecordConfirmation(t.Context(), "invalid")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := map[string]map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			if got[m.Name] == nil {
				got[m.Name] = map[string]int64{}
			}
			for _, dp := range sum.DataPoints {
				var key string
				for _, kv := range dp.Attributes.ToSlice() {
					key = string(kv.Key) + "=" + kv.Value.AsString()
				}
				got[m.Name][key] += dp.Value
			}
		}
	}

	if got[MetricTurnTaintMarksTotal][AttrTaintSource+"=tool_result"] != 1 {
		t.Errorf("turn taint marks metric = %v, want tool_result=1", got[MetricTurnTaintMarksTotal])
	}
	for _, kind := range []string{"write", "exec", "carried", "wide"} {
		if got[MetricWorkspaceTaintRecordsTotal][AttrWorkspaceTaintKind+"="+kind] != 1 {
			t.Errorf("workspace taint records metric = %v, want %s=1", got[MetricWorkspaceTaintRecordsTotal], kind)
		}
	}
	for _, outcome := range []string{"applied", "invalid"} {
		if got[MetricOperationConfirmationTotal][AttrOutcome+"="+outcome] != 1 {
			t.Errorf("operation confirmation metric = %v, want %s=1", got[MetricOperationConfirmationTotal], outcome)
		}
	}

	for _, metricName := range []string{MetricTurnTaintMarksTotal, MetricWorkspaceTaintRecordsTotal, MetricOperationConfirmationTotal} {
		if len(AllowedMetricLabels(metricName)) == 0 {
			t.Errorf("AllowedMetricLabels(%q) is empty", metricName)
		}
	}
	turnAttrs := AllowedSpanAttrs(SpanTurn)
	for _, attr := range []string{AttrTurnTainted, AttrTaintSource} {
		found := false
		for _, a := range turnAttrs {
			if a == attr {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AllowedSpanAttrs(SpanTurn) missing %q", attr)
		}
	}

	t.Run("turn span records taint attributes from streamed mark event", func(t *testing.T) {
		exporter := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
		t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

		markPayload, err := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
		if err != nil {
			t.Fatalf("EncodeTaintMark: %v", err)
		}
		fr := &fakeRuntime{events: []turn.Event{
			{Kind: turn.EventKindSessionTainted, Payload: markPayload},
			{Kind: turn.EventKindTurnCompleted},
		}}
		inst, err := InstrumentRuntime(fr, tp, mp, nil)
		if err != nil {
			t.Fatalf("InstrumentRuntime: %v", err)
		}
		for _, err := range inst.Run(t.Context(), &turn.TurnRequest{
			TurnID: "t-1", SessionID: "s-1", Origin: turn.OriginTerminal,
			PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner,
		}) {
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		}
		spans := exporter.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("spans = %d, want 1", len(spans))
		}
		var gotTainted bool
		var gotSource string
		for _, kv := range spans[0].Attributes {
			if kv.Key == AttrTurnTainted {
				gotTainted = kv.Value.AsBool()
			}
			if kv.Key == AttrTaintSource {
				gotSource = kv.Value.AsString()
			}
		}
		if !gotTainted || gotSource != string(turn.TaintSourceToolResult) {
			t.Fatalf("span taint attrs = (%v, %q), want (true, tool_result)", gotTainted, gotSource)
		}
	})

	t.Run("turn span records session taint via lookup", func(t *testing.T) {
		exporter := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
		t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

		fr := &fakeRuntime{events: []turn.Event{{Kind: turn.EventKindTurnCompleted}}}
		inst, err := InstrumentRuntime(fr, tp, mp, nil, WithTaintLookup(func(_ context.Context, _, _, _ string) (turn.TaintMark, bool, error) {
			return turn.TaintMark{Source: turn.TaintSourceToolResult}, true, nil
		}))
		if err != nil {
			t.Fatalf("InstrumentRuntime: %v", err)
		}
		for _, err := range inst.Run(t.Context(), &turn.TurnRequest{
			TurnID: "t-2", SessionID: "s-1", Origin: turn.OriginTerminal,
			PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner,
		}) {
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		}
		spans := exporter.GetSpans()
		if len(spans) != 1 {
			t.Fatalf("spans = %d, want 1", len(spans))
		}
		var gotTainted bool
		var gotSource string
		for _, kv := range spans[0].Attributes {
			if kv.Key == AttrTurnTainted {
				gotTainted = kv.Value.AsBool()
			}
			if kv.Key == AttrTaintSource {
				gotSource = kv.Value.AsString()
			}
		}
		if !gotTainted || gotSource != string(turn.TaintSourceSession) {
			t.Fatalf("span taint attrs = (%v, %q), want (true, session)", gotTainted, gotSource)
		}
	})
}
