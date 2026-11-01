package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestIngressRecorderCountsDropsByChannelAndReasonOnly(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewIngressRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewIngressRecorder(): %v", err)
	}
	recorder.Dropped(t.Context(), "discord", "not_mentioned")
	recorder.Dropped(t.Context(), "discord", "not_mentioned")
	recorder.Dropped(t.Context(), "discord", "audience")
	var nilRecorder *IngressRecorder
	nilRecorder.Dropped(t.Context(), "discord", "audience")
	allowed := map[string]bool{AttrPrincipalChannel: true, AttrIngressDropReason: true}
	totals := map[string]int64{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != MetricIngressDroppedTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("dropped data = %T, want int64 sum", point.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, attr := range dp.Attributes.ToSlice() {
					if !allowed[string(attr.Key)] {
						t.Errorf("dropped carries non-vocabulary label %q", attr.Key)
					}
				}
				reason, _ := dp.Attributes.Value(attribute.Key(AttrIngressDropReason))
				totals[reason.AsString()] += dp.Value
			}
		}
	}
	if totals["not_mentioned"] != 2 || totals["audience"] != 1 {
		t.Fatalf("dropped totals = %v, want not_mentioned=2 audience=1", totals)
	}
}
