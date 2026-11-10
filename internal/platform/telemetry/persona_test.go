package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestPersonaRecorderCountsByAgentOnly(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewPersonaRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewPersonaRecorder(): %v", err)
	}
	recorder.Turn(t.Context(), "kuma")
	recorder.Turn(t.Context(), "kuma")
	recorder.Turn(t.Context(), "main")
	recorder.Turn(t.Context(), "")
	var nilRecorder *PersonaRecorder
	nilRecorder.Turn(t.Context(), "main")
	totals := map[string]int64{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != MetricPersonaTurnsTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("persona turns data = %T, want int64 sum", point.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, attr := range dp.Attributes.ToSlice() {
					if attr.Key != AttrAgentID {
						t.Errorf("persona turns carries non-vocabulary label %q", attr.Key)
					}
				}
				agent, _ := dp.Attributes.Value(attribute.Key(AttrAgentID))
				totals[agent.AsString()] += dp.Value
			}
		}
	}
	if len(totals) != 2 || totals["kuma"] != 2 || totals["main"] != 1 {
		t.Errorf("persona turns = %v, want kuma 2 main 1", totals)
	}
}
