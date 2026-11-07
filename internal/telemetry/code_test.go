package telemetry

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type codePoint struct {
	attrs map[string]string
	count uint64
	sum   float64
}

func collectCodeMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string][]codePoint {
	t.Helper()
	var resource metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &resource); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	points := map[string][]codePoint{}
	flatten := func(set attribute.Set) map[string]string {
		attrs := map[string]string{}
		for _, kv := range set.ToSlice() {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		return attrs
	}
	for _, scope := range resource.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					points[m.Name] = append(points[m.Name], codePoint{attrs: flatten(dp.Attributes), count: uint64(dp.Value), sum: float64(dp.Value)})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					points[m.Name] = append(points[m.Name], codePoint{attrs: flatten(dp.Attributes), count: dp.Count, sum: dp.Sum})
				}
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					points[m.Name] = append(points[m.Name], codePoint{attrs: flatten(dp.Attributes), count: dp.Count, sum: float64(dp.Sum)})
				}
			}
		}
	}
	return points
}

func TestCodeRecorderRecordsPhasesResultClassesAndBuckets(t *testing.T) {
	cases := []struct {
		name        string
		observation CodeObservation
		wantRuns    map[string]string
		wantPhases  []string
		wantOutput  float64
		wantArtfcts int
	}{
		{
			name: "interpreted run",
			observation: CodeObservation{
				Runtime: "python", ResultClass: "ok", Phase: CodePhaseRun,
				RunDuration: 1500 * time.Millisecond, RunCPU: 400 * time.Millisecond, OutputBytes: 2048, ArtifactBytes: 4096,
			},
			wantRuns:    map[string]string{AttrCodeRuntime: "python", AttrCodeResultClass: "ok", AttrCodePhase: CodePhaseRun},
			wantPhases:  []string{CodePhaseRun},
			wantOutput:  2048,
			wantArtfcts: 1,
		},
		{
			name: "compiled run",
			observation: CodeObservation{
				Runtime: "go", ResultClass: "exit_nonzero", Phase: CodePhaseRun, Compiled: true,
				BuildDuration: 4 * time.Second, RunDuration: 20 * time.Millisecond, BuildCPU: 6 * time.Second, RunCPU: 10 * time.Millisecond, OutputBytes: 10,
			},
			wantRuns:    map[string]string{AttrCodeRuntime: "go", AttrCodeResultClass: "exit_nonzero", AttrCodePhase: CodePhaseRun},
			wantPhases:  []string{CodePhaseBuild, CodePhaseRun},
			wantOutput:  10,
			wantArtfcts: 1,
		},
		{
			name: "failed build",
			observation: CodeObservation{
				Runtime: "go", ResultClass: "exit_nonzero", Phase: CodePhaseBuild, Compiled: true,
				BuildDuration: 2 * time.Second, BuildCPU: 3 * time.Second, OutputBytes: 300,
			},
			wantRuns:    map[string]string{AttrCodeRuntime: "go", AttrCodeResultClass: "exit_nonzero", AttrCodePhase: CodePhaseBuild},
			wantPhases:  []string{CodePhaseBuild},
			wantOutput:  300,
			wantArtfcts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			recorder, err := NewCodeRecorder(provider)
			if err != nil {
				t.Fatalf("NewCodeRecorder: %v", err)
			}
			recorder.Record(t.Context(), &tc.observation)
			points := collectCodeMetrics(t, reader)
			runs := points[MetricCodeRunsTotal]
			if len(runs) != 1 || runs[0].count != 1 {
				t.Fatalf("runs = %+v, want one", runs)
			}
			for key, want := range tc.wantRuns {
				if runs[0].attrs[key] != want {
					t.Errorf("runs label %q = %q, want %q", key, runs[0].attrs[key], want)
				}
			}
			var phases []string
			for _, point := range points[MetricCodeRunDuration] {
				phases = append(phases, point.attrs[AttrCodePhase])
			}
			slices.Sort(phases)
			if !slices.Equal(phases, tc.wantPhases) {
				t.Errorf("duration phases = %v, want %v", phases, tc.wantPhases)
			}
			if got := len(points[MetricCodeRunCPU]); got != len(tc.wantPhases) {
				t.Errorf("cpu points = %d, want %d", got, len(tc.wantPhases))
			}
			if output := points[MetricCodeRunOutput]; len(output) != 1 || output[0].sum != tc.wantOutput {
				t.Errorf("output = %+v, want one point of %v bytes", output, tc.wantOutput)
			}
			if got := len(points[MetricCodeRunArtifacts]); got != tc.wantArtfcts {
				t.Errorf("artifact points = %d, want %d", got, tc.wantArtfcts)
			}
		})
	}
}

func TestCodeRecorderLabelsAreTheDeclaredClosedSet(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	recorder, err := NewCodeRecorder(provider)
	if err != nil {
		t.Fatalf("NewCodeRecorder: %v", err)
	}
	recorder.Record(t.Context(), &CodeObservation{
		Runtime: "go", ResultClass: "ok", Phase: CodePhaseRun, Compiled: true, OutputBytes: 5, ArtifactBytes: 5,
	})
	for name, points := range collectCodeMetrics(t, reader) {
		allowed := AllowedMetricLabels(name)
		if len(allowed) == 0 {
			t.Errorf("metric %q has no declared labels", name)
		}
		for _, point := range points {
			var got []string
			for key := range point.attrs {
				got = append(got, key)
			}
			slices.Sort(got)
			want := slices.Sorted(slices.Values(allowed))
			if !slices.Equal(got, want) {
				t.Errorf("metric %q labels = %v, want %v", name, got, want)
			}
		}
		for _, label := range allowed {
			if label == AttrSessionID || label == AttrTurnID {
				t.Errorf("metric %q carries high-cardinality label %q", name, label)
			}
		}
	}
}

func TestCodeRecorderIgnoresNilReceiversAndObservations(t *testing.T) {
	var nilRecorder *CodeRecorder
	nilRecorder.Record(t.Context(), &CodeObservation{Runtime: "python"})
	recorder, err := NewCodeRecorder(nil)
	if err != nil {
		t.Fatalf("NewCodeRecorder: %v", err)
	}
	recorder.Record(t.Context(), nil)
}
