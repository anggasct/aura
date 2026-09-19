package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestProfileRecorderEmitsExtractionExpiryContextMetrics(t *testing.T) {
	reader := newTestManualReader()
	recorder, err := NewProfileRecorder(newTestMeterProvider(reader), ProfileCallbacks{})
	if err != nil {
		t.Fatalf("NewProfileRecorder(): %v", err)
	}
	ctx := context.Background()
	recorder.Record(ctx, &ProfileObservation{Kind: "extraction", Result: "completed", QueueAge: 250 * time.Millisecond, Accepted: 2})
	recorder.Record(ctx, &ProfileObservation{Kind: "extraction", Result: "dropped"})
	recorder.Record(ctx, &ProfileObservation{Kind: "expiry", Lag: time.Hour})
	recorder.Record(ctx, &ProfileObservation{Kind: "context", Facts: 4, Tokens: 512})
	recorder.Record(ctx, nil)
	points := map[string]bool{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			points[point.Name] = true
		}
	}
	for _, want := range []string{
		MetricProfileExtractionsTotal, MetricProfileExtractionQueueAge,
		MetricProfileExpiryLag, MetricProfileContextFacts, MetricProfileContextTokens,
	} {
		if !points[want] {
			t.Errorf("metric %q missing", want)
		}
	}
}

func TestProfileRecorderObservableGauges(t *testing.T) {
	reader := newTestManualReader()
	depth := int64(3)
	recorder, err := NewProfileRecorder(newTestMeterProvider(reader), ProfileCallbacks{
		QueueDepth: func() int64 { return depth },
		FactCounts: func() []ProfileFactCount {
			return []ProfileFactCount{
				{Status: "active", Category: "tool", Count: 2},
				{Status: "candidate", Category: "language", Count: 5},
			}
		},
	})
	if err != nil {
		t.Fatalf("NewProfileRecorder(): %v", err)
	}
	recorder.Record(context.Background(), &ProfileObservation{Kind: "extraction", Result: "completed", QueueAge: time.Millisecond})
	recorder.Record(context.Background(), &ProfileObservation{Kind: "expiry", Lag: time.Second})
	recorder.Record(context.Background(), &ProfileObservation{Kind: "context", Facts: 1, Tokens: 8})
	units := map[string]string{}
	for _, scope := range collectMetrics(t, reader).ScopeMetrics {
		for _, point := range scope.Metrics {
			units[point.Name] = point.Unit
		}
	}
	if units[MetricProfileExtractionQueueDepth] != "{job}" {
		t.Errorf("queue depth unit = %q, want {job}", units[MetricProfileExtractionQueueDepth])
	}
	if units[MetricProfileFactsCount] != "{fact}" {
		t.Errorf("facts count unit = %q, want {fact}", units[MetricProfileFactsCount])
	}
	wantUnits := map[string]string{
		MetricProfileExtractionQueueAge: "s",
		MetricProfileExpiryLag:          "s",
		MetricProfileContextFacts:       "{fact}",
		MetricProfileContextTokens:      "{token}",
	}
	for name, want := range wantUnits {
		if units[name] != want {
			t.Errorf("%s unit = %q, want %q", name, units[name], want)
		}
	}
}

func TestProfileMetricLabelsExcludeHighCardinality(t *testing.T) {
	for metric, labels := range map[string][]string{
		MetricProfileExtractionsTotal:     AllowedMetricLabels(MetricProfileExtractionsTotal),
		MetricProfileExtractionQueueAge:   AllowedMetricLabels(MetricProfileExtractionQueueAge),
		MetricProfileExtractionQueueDepth: AllowedMetricLabels(MetricProfileExtractionQueueDepth),
		MetricProfileFactsCount:           AllowedMetricLabels(MetricProfileFactsCount),
		MetricProfileExpiryLag:            AllowedMetricLabels(MetricProfileExpiryLag),
		MetricProfileContextFacts:         AllowedMetricLabels(MetricProfileContextFacts),
		MetricProfileContextTokens:        AllowedMetricLabels(MetricProfileContextTokens),
	} {
		for _, label := range labels {
			if label == AttrSessionID || label == AttrTurnID {
				t.Errorf("metric %q carries high-cardinality label %q", metric, label)
			}
		}
	}
}
