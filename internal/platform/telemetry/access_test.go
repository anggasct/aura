package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestBudgetExhaustedCountsByClassAndLimitOnly(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewAccessRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewAccessRecorder(): %v", err)
	}
	recorder.BudgetExhausted(t.Context(), "public", "tokens_per_day")
	recorder.BudgetExhausted(t.Context(), "public", "turns_per_minute")
	recorder.BudgetExhausted(t.Context(), "trusted", "turns_per_minute")
	var nilRecorder *AccessRecorder
	nilRecorder.BudgetExhausted(t.Context(), "public", "tokens_per_day")
	allowed := map[string]bool{AttrPrincipalClass: true, AttrBudgetLimit: true}
	totals := map[string]int64{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name != MetricBudgetExhaustedTotal {
				continue
			}
			sum, ok := point.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("budget data = %T, want int64 sum", point.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, attr := range dp.Attributes.ToSlice() {
					if !allowed[string(attr.Key)] {
						t.Errorf("budget counter carries non-vocabulary label %q", attr.Key)
					}
				}
				class, _ := dp.Attributes.Value(attribute.Key(AttrPrincipalClass))
				limit, _ := dp.Attributes.Value(attribute.Key(AttrBudgetLimit))
				totals[class.AsString()+"/"+limit.AsString()] += dp.Value
			}
		}
	}
	if totals["public/tokens_per_day"] != 1 || totals["public/turns_per_minute"] != 1 || totals["trusted/turns_per_minute"] != 1 || len(totals) != 3 {
		t.Fatalf("budget totals = %v", totals)
	}
}
