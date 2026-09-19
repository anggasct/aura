package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type ProfileObservation struct {
	Kind     string
	Result   string
	QueueAge time.Duration
	Lag      time.Duration
	Accepted int
	Screened int
	Skipped  int
	Facts    int
	Tokens   int
}

type ProfileFactCount struct {
	Status   string
	Category string
	Count    int64
}

type ProfileCallbacks struct {
	QueueDepth func() int64
	FactCounts func() []ProfileFactCount
}

type ProfileRecorder struct {
	extractions  metric.Int64Counter
	queueAge     metric.Float64Histogram
	expiryLag    metric.Float64Histogram
	contextFacts metric.Float64Histogram
	contextToken metric.Float64Histogram
}

func NewProfileRecorder(mp metric.MeterProvider, callbacks ProfileCallbacks) (*ProfileRecorder, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(ScopeName)
	var err error
	recorder := &ProfileRecorder{}
	if recorder.extractions, err = meter.Int64Counter(MetricProfileExtractionsTotal,
		metric.WithDescription("profile extraction job outcomes by result class")); err != nil {
		return nil, fmt.Errorf("telemetry: create profile extractions counter: %w", err)
	}
	if recorder.queueAge, err = meter.Float64Histogram(MetricProfileExtractionQueueAge,
		metric.WithUnit("s"),
		metric.WithDescription("profile extraction job queue age at dequeue in seconds by result class")); err != nil {
		return nil, fmt.Errorf("telemetry: create profile queue age histogram: %w", err)
	}
	if recorder.expiryLag, err = meter.Float64Histogram(MetricProfileExpiryLag,
		metric.WithUnit("s"),
		metric.WithDescription("profile fact lateness at expiry sweep in seconds")); err != nil {
		return nil, fmt.Errorf("telemetry: create profile expiry lag histogram: %w", err)
	}
	if recorder.contextFacts, err = meter.Float64Histogram(MetricProfileContextFacts,
		metric.WithUnit("{fact}"),
		metric.WithDescription("profile facts admitted into untrusted context per retrieval")); err != nil {
		return nil, fmt.Errorf("telemetry: create profile context facts histogram: %w", err)
	}
	if recorder.contextToken, err = meter.Float64Histogram(MetricProfileContextTokens,
		metric.WithUnit("{token}"),
		metric.WithDescription("profile context token usage per retrieval")); err != nil {
		return nil, fmt.Errorf("telemetry: create profile context tokens histogram: %w", err)
	}
	if callbacks.QueueDepth != nil {
		if _, err = meter.Int64ObservableGauge(MetricProfileExtractionQueueDepth,
			metric.WithUnit("{job}"),
			metric.WithDescription("profile extraction jobs waiting in the bounded queue"),
			metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
				observer.Observe(callbacks.QueueDepth())
				return nil
			})); err != nil {
			return nil, fmt.Errorf("telemetry: create profile queue depth gauge: %w", err)
		}
	}
	if callbacks.FactCounts != nil {
		if _, err = meter.Int64ObservableGauge(MetricProfileFactsCount,
			metric.WithUnit("{fact}"),
			metric.WithDescription("profile facts by lifecycle status and category"),
			metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
				for _, count := range callbacks.FactCounts() {
					observer.Observe(count.Count, metric.WithAttributes(
						attribute.String(AttrProfileStatus, count.Status),
						attribute.String(AttrProfileCategory, count.Category),
					))
				}
				return nil
			})); err != nil {
			return nil, fmt.Errorf("telemetry: create profile facts gauge: %w", err)
		}
	}
	return recorder, nil
}

func (r *ProfileRecorder) Record(ctx context.Context, observation *ProfileObservation) {
	if r == nil || observation == nil {
		return
	}
	switch observation.Kind {
	case "extraction":
		resultOpt := metric.WithAttributes(attribute.String(AttrProfileResult, observation.Result))
		r.extractions.Add(ctx, 1, resultOpt)
		r.queueAge.Record(ctx, observation.QueueAge.Seconds(), resultOpt)
	case "expiry":
		r.expiryLag.Record(ctx, observation.Lag.Seconds())
	case "context":
		r.contextFacts.Record(ctx, float64(observation.Facts))
		r.contextToken.Record(ctx, float64(observation.Tokens))
	}
}
