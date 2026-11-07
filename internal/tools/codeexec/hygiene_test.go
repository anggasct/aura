package codeexec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
)

const hygieneMarker = "zqxmark7f3a91c2"

func hygieneOutputName() string {
	return hygieneMarker + ".txt"
}

func (h *auditHarness) runWithMarkers(runtime, source string) encodedResult {
	h.t.Helper()
	result, err := h.call(map[string]any{
		"runtime": runtime, "source": source, "stdin": "stdin " + hygieneMarker, "output_files": []string{hygieneOutputName()},
	})
	if err != nil {
		h.t.Fatalf("code_execute(%s): %v", runtime, err)
	}
	var decoded encodedResult
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		h.t.Fatal(err)
	}
	return decoded
}

func (h *auditHarness) assertReceiptsMarkerFree() {
	h.t.Helper()
	intents := succeededIntents(h.t, h)
	if len(intents) == 0 {
		h.t.Fatal("no effect was journaled")
	}
	for i := range intents {
		if strings.Contains(string(intents[i].ProviderReceipt), hygieneMarker) {
			h.t.Errorf("the journaled receipt carries the marker: %s", intents[i].ProviderReceipt)
		}
	}
}

func TestLogsAndTelemetryNeverCarryProgramContent(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		results []sandbox.Result
	}{
		{"python", "python", []sandbox.Result{{Stdout: "stdout " + hygieneMarker, Stderr: "stderr " + hygieneMarker}}},
		{"javascript", "javascript", []sandbox.Result{{Stdout: "stdout " + hygieneMarker, Stderr: "stderr " + hygieneMarker}}},
		{"go", "go", []sandbox.Result{{}, {Stdout: "stdout " + hygieneMarker, Stderr: "stderr " + hygieneMarker}}},
		{"non-zero exit", "python", []sandbox.Result{{ExitCode: 1, Stdout: "stdout " + hygieneMarker, Stderr: "stderr " + hygieneMarker}}},
		{"failed go build", "go", []sandbox.Result{{ExitCode: 1, Stderr: "stderr " + hygieneMarker}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.script = tc.results
			rig.stub.before = writesOutputs(t, map[string]string{hygieneOutputName(): hygieneMarker})
			harness := newAuditHarness(t, rig.runner, t.TempDir(), 1<<20)
			decoded := harness.runWithMarkers(tc.runtime, "# source "+hygieneMarker)
			if !strings.Contains(decoded.Stderr, hygieneMarker) {
				t.Fatalf("the marker never reached the result, so the check proves nothing: %+v", decoded)
			}
			harness.assertMarkerFree(hygieneMarker)
			harness.assertReceiptsMarkerFree()
		})
	}
}

func TestRefusalsNeverEchoProgramContent(t *testing.T) {
	cases := []struct {
		name      string
		arguments map[string]any
	}{
		{"oversized source", map[string]any{"runtime": "python", "source": strings.Repeat("a", 2<<10) + hygieneMarker}},
		{"oversized stdin", map[string]any{"runtime": "python", "source": "x", "stdin": strings.Repeat("a", 2<<10) + hygieneMarker}},
		{"invalid output name", map[string]any{"runtime": "python", "source": "x", "output_files": []string{"../" + hygieneMarker}}},
		{"duplicate output name", map[string]any{"runtime": "python", "source": "x", "output_files": []string{hygieneOutputName(), hygieneOutputName()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			harness := newAuditHarness(t, rig.runner, t.TempDir(), 1<<20)
			_, err := harness.call(tc.arguments)
			if err == nil {
				t.Fatal("the request was accepted")
			}
			if strings.Contains(err.Error(), hygieneMarker) {
				t.Errorf("the refusal echoes program content: %v", err)
			}
			for _, line := range harness.logs.all() {
				if strings.Contains(line, hygieneMarker) {
					t.Errorf("log record carries program content: %s", line)
				}
			}
			if rig.stub.count() != 0 {
				t.Errorf("a refused request started %d runs", rig.stub.count())
			}
		})
	}
}
