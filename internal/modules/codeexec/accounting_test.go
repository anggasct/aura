package codeexec

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/platform/sandbox"
)

func TestDurationIsBuildPlusRun(t *testing.T) {
	cases := []struct {
		name      string
		runtime   string
		delays    []time.Duration
		cpu       []time.Duration
		wantBuild time.Duration
		wantRun   time.Duration
		wantPhase Phase
	}{
		{"interpreted", "python", []time.Duration{2 * time.Second}, []time.Duration{time.Second}, 0, 2 * time.Second, PhaseRun},
		{"compiled", "go", []time.Duration{3 * time.Second, 4 * time.Second}, []time.Duration{5 * time.Second, time.Second}, 3 * time.Second, 4 * time.Second, PhaseRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rig := newRig(t, nil)
				var seen []Observation
				rig.runner.observer = func(_ context.Context, observation *Observation) { seen = append(seen, *observation) }
				results := make([]sandbox.Result, len(tc.cpu))
				for i, cpu := range tc.cpu {
					results[i] = sandbox.Result{CPUTime: cpu}
				}
				rig.stub.script = results
				delays := tc.delays
				rig.stub.before = func(context.Context, *sandbox.Spec) {
					time.Sleep(delays[0])
					delays = delays[1:]
				}
				outcome, err := rig.runner.Execute(t.Context(), &Request{Runtime: tc.runtime, Source: "x"})
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Build.Duration != tc.wantBuild || outcome.Run.Duration != tc.wantRun || outcome.Phase != tc.wantPhase {
					t.Fatalf("build = %v, run = %v, phase = %s", outcome.Build.Duration, outcome.Run.Duration, outcome.Phase)
				}
				if outcome.Duration != outcome.Build.Duration+outcome.Run.Duration {
					t.Fatalf("duration %v is not build %v plus run %v", outcome.Duration, outcome.Build.Duration, outcome.Run.Duration)
				}
				encoded, err := encodeBounded(&outcome, &rig.runner.limits, 64<<10)
				if err != nil {
					t.Fatal(err)
				}
				var decoded encodedResult
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				if want := (tc.wantBuild + tc.wantRun).Milliseconds(); decoded.DurationMS != want {
					t.Fatalf("reported duration_ms = %d, want %d", decoded.DurationMS, want)
				}
				if len(seen) != 1 || seen[0].Build.Duration != tc.wantBuild || seen[0].Run.Duration != tc.wantRun ||
					seen[0].Build.CPUTime+seen[0].Run.CPUTime != sumDurations(tc.cpu) {
					t.Fatalf("observation = %+v", seen)
				}
			})
		})
	}
}

func sumDurations(values []time.Duration) time.Duration {
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return total
}

func TestArtifactBytesAndQuotaUsageReconcile(t *testing.T) {
	cases := []struct {
		name      string
		quota     int64
		files     map[string]string
		declared  []string
		wantSizes int64
		wantQuota int64
		refused   int
	}{
		{
			name: "distinct files", quota: 1 << 20,
			files:    map[string]string{"a.txt": "alpha", "b.txt": "bravo-bravo", "c.txt": "c"},
			declared: []string{"a.txt", "b.txt", "c.txt"}, wantSizes: 17, wantQuota: 17,
		},
		{
			name: "identical content shares one blob", quota: 1 << 20,
			files:    map[string]string{"a.txt": "same", "b.txt": "same"},
			declared: []string{"a.txt", "b.txt"}, wantSizes: 8, wantQuota: 4,
		},
		{
			name: "quota exhaustion keeps only what fit", quota: 10,
			files:    map[string]string{"a.txt": "12345", "b.txt": "1234567890", "c.txt": "abcde"},
			declared: []string{"a.txt", "b.txt", "c.txt"}, wantSizes: 10, wantQuota: 10, refused: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.before = writesOutputs(t, tc.files)
			harness := newAuditHarness(t, rig.runner, t.TempDir(), tc.quota)
			before := harness.quota()
			result, err := harness.call(map[string]any{"runtime": "python", "source": "x", "output_files": tc.declared})
			if err != nil {
				t.Fatal(err)
			}
			var returned encodedResult
			if err := json.Unmarshal(result.Output, &returned); err != nil {
				t.Fatal(err)
			}
			var reportedBytes int64
			distinct := map[string]int64{}
			for _, artifact := range returned.Artifacts {
				if artifact.Refused == "" {
					reportedBytes += artifact.Size
					distinct[artifact.Digest] = artifact.Size
				}
			}
			var distinctBytes int64
			for _, size := range distinct {
				distinctBytes += size
			}
			observed := harness.observed.all()
			if len(observed) != 1 {
				t.Fatalf("observations = %d, want 1", len(observed))
			}
			if observed[0].ArtifactBytes != reportedBytes || observed[0].Refused != tc.refused {
				t.Errorf("observed artifact bytes = %d, refused = %d; result reports %d bytes and want %d refused", observed[0].ArtifactBytes, observed[0].Refused, reportedBytes, tc.refused)
			}
			intents := succeededIntents(t, harness)
			var receipt receiptView
			if err := json.Unmarshal(intents[0].ProviderReceipt, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Audit.ArtifactBytes != reportedBytes {
				t.Errorf("audited artifact bytes = %d, result reports %d", receipt.Audit.ArtifactBytes, reportedBytes)
			}
			if reportedBytes != tc.wantSizes {
				t.Errorf("reported artifact bytes = %d, want %d", reportedBytes, tc.wantSizes)
			}
			if rise := harness.quota() - before; rise != distinctBytes || rise != tc.wantQuota {
				t.Errorf("quota usage rose by %d, distinct imported bytes = %d, want %d", rise, distinctBytes, tc.wantQuota)
			}
		})
	}
}
