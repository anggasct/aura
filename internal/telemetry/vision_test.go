package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestVisionRecorderUsesBoundedLabels(t *testing.T) {
	recorder, err := NewVisionRecorder(nil)
	if err != nil {
		t.Fatalf("NewVisionRecorder: %v", err)
	}
	recorder.Record(context.Background(), &VisionObservation{
		Operation: "ingest",
		Result:    "ok",
		MIME:      "image/png",
		Images:    1,
		Version:   "v1",
		Protocol:  "",
		Duration:  10 * time.Millisecond,
	})
	recorder.Record(context.Background(), nil)
	var nilRecorder *VisionRecorder
	nilRecorder.Record(context.Background(), &VisionObservation{Operation: "ingest"})
	labels := AllowedMetricLabels(MetricVisionOperationsTotal)
	for _, want := range []string{AttrVisionOperation, AttrVisionResult, AttrVisionMIME, AttrVisionVersion, AttrVisionProtocol} {
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
