package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestVisionRecorderUsesBoundedLabels(t *testing.T) {
	recorder, err := NewVisionRecorder(nil)
	if err != nil {
		t.Fatalf("NewVisionRecorder: %v", err)
	}
	recorder.Record(context.Background(), &VisionObservation{
		Operation:    "ingest",
		Result:       "ok",
		MIME:         "image/png",
		Images:       1,
		Version:      "v1",
		Protocol:     "",
		SizeBucket:   1 << 16,
		PixelsBucket: 1 << 18,
		Duration:     10 * time.Millisecond,
	})
	recorder.Record(context.Background(), nil)
	var nilRecorder *VisionRecorder
	nilRecorder.Record(context.Background(), &VisionObservation{Operation: "ingest"})
	labels := AllowedMetricLabels(MetricVisionOperationsTotal)
	for _, want := range []string{AttrVisionOperation, AttrVisionResult, AttrVisionMIME, AttrVisionVersion, AttrVisionProtocol, AttrVisionImages, AttrVisionSizeBucket, AttrVisionPixelsBucket} {
		found := false
		for _, got := range labels {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("vision operations labels missing %q: %v", want, labels)
		}
	}
	for _, forbidden := range []string{AttrSessionID, AttrTurnID, "vision.digest", "vision.artifact_id", "vision.filename"} {
		for _, got := range labels {
			if got == forbidden {
				t.Errorf("vision labels carry high-cardinality %q", forbidden)
			}
		}
	}
	durationLabels := AllowedMetricLabels(MetricVisionOperationDuration)
	if len(durationLabels) == 0 {
		t.Fatal("vision duration labels missing")
	}
	for _, want := range []string{AttrVisionImages, AttrVisionSizeBucket, AttrVisionPixelsBucket} {
		found := false
		for _, got := range durationLabels {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("vision duration labels missing %q: %v", want, durationLabels)
		}
	}
}

func TestVisionRecorderEmitsBucketLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	recorder, err := NewVisionRecorder(mp)
	if err != nil {
		t.Fatalf("NewVisionRecorder: %v", err)
	}
	recorder.Record(context.Background(), &VisionObservation{
		Operation:    "ingest",
		Result:       "ok",
		MIME:         "image/png",
		Images:       1,
		Version:      "v1",
		Protocol:     "",
		SizeBucket:   1 << 16,
		PixelsBucket: 1 << 18,
		Duration:     10 * time.Millisecond,
	})
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	foundOps, foundDuration := false, false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case MetricVisionOperationsTotal:
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key(AttrVisionImages)); ok && v.AsInt64() == 1 {
						if _, ok := dp.Attributes.Value(attribute.Key(AttrVisionSizeBucket)); ok {
							if _, ok := dp.Attributes.Value(attribute.Key(AttrVisionPixelsBucket)); ok {
								foundOps = true
							}
						}
					}
				}
			case MetricVisionOperationDuration:
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					continue
				}
				for _, dp := range hist.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key(AttrVisionImages)); ok && v.AsInt64() == 1 {
						if _, ok := dp.Attributes.Value(attribute.Key(AttrVisionSizeBucket)); ok {
							if _, ok := dp.Attributes.Value(attribute.Key(AttrVisionPixelsBucket)); ok {
								foundDuration = true
							}
						}
					}
				}
			}
		}
	}
	if !foundOps {
		t.Error("vision operations counter is missing images/size/pixels bucket labels")
	}
	if !foundDuration {
		t.Error("vision duration histogram is missing images/size/pixels bucket labels")
	}
}

func TestVisionObservationExcludesContent(t *testing.T) {
	obs := &VisionObservation{
		Operation: "ingest",
		Result:    "ok",
		MIME:      "image/png",
		Images:    1,
		Version:   "v1",
		Protocol:  "",
		Duration:  time.Millisecond,
	}
	if obs.Operation != "ingest" || obs.MIME != "image/png" {
		t.Fatalf("observation = %+v", obs)
	}
}
