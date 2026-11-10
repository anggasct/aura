package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestPrincipalRecorderCountsByClassAndChannelOnly(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewPrincipalRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewPrincipalRecorder(): %v", err)
	}
	recorder.Admitted(t.Context(), "owner", "discord")
	recorder.Admitted(t.Context(), "owner", "discord")
	recorder.Admitted(t.Context(), "trusted", "discord")
	var nilRecorder *PrincipalRecorder
	nilRecorder.Admitted(t.Context(), "owner", "discord")
	allowed := map[string]bool{AttrPrincipalClass: true, AttrPrincipalChannel: true}
	totals := map[string]int64{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != MetricPrincipalAdmissionsTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("admissions data = %T, want int64 sum", point.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, attr := range dp.Attributes.ToSlice() {
					if !allowed[string(attr.Key)] {
						t.Errorf("admissions carries non-vocabulary label %q", attr.Key)
					}
				}
				class, _ := dp.Attributes.Value(attribute.Key(AttrPrincipalClass))
				totals[class.AsString()] += dp.Value
			}
		}
	}
	if totals["owner"] != 2 || totals["trusted"] != 1 {
		t.Errorf("admissions = %v, want owner 2 trusted 1", totals)
	}
}
