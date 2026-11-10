package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestMemoryRecorderEmitsRecallMetrics(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewMemoryRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewMemoryRecorder(): %v", err)
	}
	ctx := context.Background()
	recorder.Record(ctx, &MemoryObservation{Outcome: "ok", ModelSystem: "anthropic_messages", Documents: 3, Duration: 12 * time.Millisecond})
	recorder.Record(ctx, &MemoryObservation{Outcome: "memory_budget_invalid", Duration: time.Millisecond})
	recorder.Record(ctx, nil)
	names := map[string]bool{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			names[point.Name] = true
		}
	}
	for _, want := range []string{MetricMemoryRecallsTotal, MetricMemoryRecallDuration, MetricMemoryRecallDocuments} {
		if !names[want] {
			t.Errorf("metric %q missing", want)
		}
	}
}

func TestMemoryRecorderIgnoresNil(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewMemoryRecorder(newTestMeterProvider(reader))
	if err != nil {
		t.Fatalf("NewMemoryRecorder(): %v", err)
	}
	var nilRecorder *MemoryRecorder
	nilRecorder.Record(context.Background(), &MemoryObservation{Outcome: "ok"})
	recorder.Record(context.Background(), nil)
}

func TestMemoryMetricLabelsExcludeHighCardinality(t *testing.T) {
	for metric, labels := range map[string][]string{
		MetricMemoryRecallsTotal:    AllowedMetricLabels(MetricMemoryRecallsTotal),
		MetricMemoryRecallDuration:  AllowedMetricLabels(MetricMemoryRecallDuration),
		MetricMemoryRecallDocuments: AllowedMetricLabels(MetricMemoryRecallDocuments),
	} {
		for _, label := range labels {
			if label == AttrSessionID || label == AttrTurnID || label == AttrRecallSources || label == AttrRecallScores {
				t.Errorf("metric %q carries high-cardinality label %q", metric, label)
			}
		}
	}
}

func TestMemorySpanAllowedAttrs(t *testing.T) {
	allowed := AllowedSpanAttrs(SpanMemoryRecall)
	want := []string{
		AttrRecallOutcome,
		AttrRecallSummarized,
		AttrRecallDocuments,
		AttrRecallScreened,
		AttrRecallScores,
		AttrRecallSources,
		AttrRecallModelSystem,
		AttrRecallModelName,
		AttrRecallSummaryVersion,
		AttrSemconvVersion,
	}
	assertAttrSet(t, "memory recall span", allowed, want)
	for _, key := range allowed {
		if isContentLeak(key) {
			t.Errorf("memory recall span key %q trips the content blocklist", key)
		}
	}
}

func TestPrivacyCanaryMemorySpanKeys(t *testing.T) {
	for _, key := range AllowedSpanAttrs(SpanMemoryRecall) {
		if isContentLeak(key) {
			t.Errorf("content-bearing key %q present in memory recall span allowlist", key)
		}
	}
}
