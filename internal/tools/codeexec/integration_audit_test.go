//go:build linux && integration

package codeexec

import (
	"encoding/json"
	"strings"
	"testing"
)

func hygieneSources() map[string]string {
	name := hygieneOutputName()
	return map[string]string{
		"python": "import os, sys\ndata = sys.stdin.read()\nos.makedirs('output', exist_ok=True)\n" +
			"open('output/" + name + "', 'w').write('" + hygieneMarker + "')\n" +
			"print('stdout " + hygieneMarker + "', data)\nprint('stderr " + hygieneMarker + "', file=sys.stderr)\n",
		"javascript": "const fs = require('fs')\nconst data = fs.readFileSync(0, 'utf8')\nfs.mkdirSync('output', { recursive: true })\n" +
			"fs.writeFileSync('output/" + name + "', '" + hygieneMarker + "')\n" +
			"console.log('stdout " + hygieneMarker + "', data)\nconsole.error('stderr " + hygieneMarker + "')\n",
		"go": "package main\nimport (\"fmt\"; \"io\"; \"os\")\nfunc main() {\n\tdata, _ := io.ReadAll(os.Stdin)\n\tos.MkdirAll(\"output\", 0o755)\n" +
			"\tos.WriteFile(\"output/" + name + "\", []byte(\"" + hygieneMarker + "\"), 0o644)\n" +
			"\tfmt.Println(\"stdout " + hygieneMarker + "\", string(data))\n\tfmt.Fprintln(os.Stderr, \"stderr " + hygieneMarker + "\")\n}\n",
	}
}

func TestLiveLogsAndTelemetryNeverCarryProgramContent(t *testing.T) {
	for runtime, source := range hygieneSources() {
		t.Run(runtime, func(t *testing.T) {
			harness := newAuditHarness(t, host.runnerWith(t, func(*Limits) {}), t.TempDir(), 1<<20)
			decoded := harness.runWithMarkers(runtime, source)
			if decoded.ResultClass != ClassOK || len(decoded.Artifacts) != 1 || decoded.Artifacts[0].Refused != "" {
				t.Fatalf("result = %+v", decoded)
			}
			if !strings.Contains(decoded.Stdout, hygieneMarker) || !strings.Contains(decoded.Stderr, hygieneMarker) {
				t.Fatalf("the marker never reached stdout and stderr, so the check proves nothing: %+v", decoded)
			}
			harness.assertMarkerFree(hygieneMarker)
			harness.assertReceiptsMarkerFree()
		})
	}
}

func TestLiveAccountingReconciles(t *testing.T) {
	sources := map[string]string{
		"python":     "open('output/a.txt', 'wb').write(b'x' * 1000)\nopen('output/b.txt', 'wb').write(b'y' * 2500)\n",
		"javascript": "const fs = require('fs')\nfs.writeFileSync('output/a.txt', Buffer.alloc(1000, 120))\nfs.writeFileSync('output/b.txt', Buffer.alloc(2500, 121))\n",
		"go": "package main\nimport (\"bytes\"; \"os\")\nfunc main() {\n\tos.MkdirAll(\"output\", 0o755)\n" +
			"\tos.WriteFile(\"output/a.txt\", bytes.Repeat([]byte(\"x\"), 1000), 0o644)\n\tos.WriteFile(\"output/b.txt\", bytes.Repeat([]byte(\"y\"), 2500), 0o644)\n}\n",
	}
	for runtime, source := range sources {
		t.Run(runtime, func(t *testing.T) {
			harness := newAuditHarness(t, host.runnerWith(t, func(*Limits) {}), t.TempDir(), 1<<20)
			before := harness.quota()
			result, err := harness.call(map[string]any{"runtime": runtime, "source": source, "output_files": []string{"a.txt", "b.txt"}})
			if err != nil {
				t.Fatal(err)
			}
			var returned encodedResult
			if err := json.Unmarshal(result.Output, &returned); err != nil {
				t.Fatal(err)
			}
			if returned.ResultClass != ClassOK || len(returned.Artifacts) != 2 {
				t.Fatalf("result = %+v", returned)
			}
			var reported int64
			for _, artifact := range returned.Artifacts {
				reported += artifact.Size
			}
			observed := harness.observed.all()
			if len(observed) != 1 {
				t.Fatalf("observations = %d, want 1", len(observed))
			}
			got := observed[0]
			if reported != 3500 || got.ArtifactBytes != reported || harness.quota()-before != reported {
				t.Errorf("reported = %d, observed = %d, quota rose by %d; want 3500 each", reported, got.ArtifactBytes, harness.quota()-before)
			}
			if got.Run.Duration <= 0 || got.Run.CPUTime <= 0 {
				t.Errorf("run usage = %+v, want a measured duration and CPU time", got.Run)
			}
			if (runtime == "go") != (got.Build.Duration > 0 && got.Build.CPUTime > 0) {
				t.Errorf("build usage = %+v for %s", got.Build, runtime)
			}
			if want := (got.Build.Duration + got.Run.Duration).Milliseconds(); returned.DurationMS != want {
				t.Errorf("reported duration_ms = %d, want build plus run = %d", returned.DurationMS, want)
			}
		})
	}
}
