package telemetry

import (
	"context"
	"testing"
)

func TestChildRecorderEmitsSpawnSettleMetrics(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewChildRecorder(newTestMeterProvider(reader), func() ChildActiveCounts {
		return ChildActiveCounts{Queued: 2, Running: 1}
	})
	if err != nil {
		t.Fatalf("NewChildRecorder(): %v", err)
	}
	ctx := context.Background()
	recorder.RecordSpawn(ctx, "created")
	recorder.RecordSpawn(ctx, "conflict")
	recorder.RecordSettle(ctx, &ChildObservation{State: "succeeded", Result: "completed", TokensUsed: 120, CostMicros: 30})
	recorder.RecordSettle(ctx, nil)
	var nilRecorder *ChildRecorder
	nilRecorder.RecordSpawn(ctx, "created")
	nilRecorder.RecordSettle(ctx, &ChildObservation{State: "failed"})
	names := map[string]bool{}
	units := map[string]string{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			names[point.Name] = true
			units[point.Name] = point.Unit
		}
	}
	for _, want := range []string{MetricChildSpawnsTotal, MetricChildSettlesTotal, MetricChildActive, MetricChildBudgetTokens, MetricChildBudgetCost} {
		if !names[want] {
			t.Errorf("metric %q missing", want)
		}
	}
	if units[MetricChildBudgetTokens] != "{token}" || units[MetricChildBudgetCost] != "{micro}" || units[MetricChildActive] != "{child}" {
		t.Errorf("units = %v", units)
	}
}

func TestChildMetricLabelsExcludeHighCardinality(t *testing.T) {
	for metric, labels := range map[string][]string{
		MetricChildSpawnsTotal:  AllowedMetricLabels(MetricChildSpawnsTotal),
		MetricChildSettlesTotal: AllowedMetricLabels(MetricChildSettlesTotal),
		MetricChildActive:       AllowedMetricLabels(MetricChildActive),
		MetricChildBudgetTokens: AllowedMetricLabels(MetricChildBudgetTokens),
		MetricChildBudgetCost:   AllowedMetricLabels(MetricChildBudgetCost),
	} {
		for _, label := range labels {
			if label == AttrSessionID || label == AttrTurnID {
				t.Errorf("metric %q carries high-cardinality label %q", metric, label)
			}
		}
	}
}
