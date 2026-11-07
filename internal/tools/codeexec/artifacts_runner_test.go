package codeexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools"
)

func writesOutputs(t *testing.T, files map[string]string) func(context.Context, *sandbox.Spec) {
	t.Helper()
	return func(_ context.Context, spec *sandbox.Spec) {
		for name, content := range files {
			target := filepath.Join(spec.WorkingDir, outputDirName, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), outputDirMode); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
}

func TestExecuteImportsDeclaredOutputsOnlyFromFinishedRuns(t *testing.T) {
	cases := []struct {
		name     string
		result   sandbox.Result
		imported bool
	}{
		{"ok", sandbox.Result{}, true},
		{"exit nonzero", sandbox.Result{ExitCode: 2}, true},
		{"timeout", sandbox.Result{Terminated: true, TimedOut: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, false},
		{"cancelled", sandbox.Result{Terminated: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, false},
		{"policy denied", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSYS)}, false},
		{"memory limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), OOMKilled: true}, false},
		{"disk limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), DiskLimited: true}, false},
		{"file limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGXFSZ), FileLimited: true}, false},
		{"process limit", sandbox.Result{ExitCode: 1, PidsLimited: true}, false},
		{"runtime crash", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSEGV)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.result = tc.result
			rig.stub.before = writesOutputs(t, map[string]string{"a.txt": "alpha"})
			outcome, err := rig.runner.Execute(t.Context(), &Request{
				Runtime: "python", Source: "x", SessionID: "session-9", RequestID: "request-9", OutputFiles: []string{"a.txt", "b.txt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			puts := rig.sink.recorded()
			if !tc.imported {
				if outcome.Artifacts != nil || len(puts) != 0 {
					t.Fatalf("a %s run imported %+v / %d puts", outcome.Class, outcome.Artifacts, len(puts))
				}
				return
			}
			if len(outcome.Artifacts) != 2 || outcome.Artifacts[0].Refused != "" || outcome.Artifacts[1].Refused != RefusalAbsent {
				t.Fatalf("artifacts = %+v", outcome.Artifacts)
			}
			if len(puts) != 1 || string(puts[0].content) != "alpha" || puts[0].meta.SessionID != "session-9" {
				t.Fatalf("puts = %+v", puts)
			}
			var provenance map[string]string
			if err := json.Unmarshal(puts[0].meta.Metadata, &provenance); err != nil {
				t.Fatal(err)
			}
			if provenance["kind"] != "code_execute_output" || provenance["runtime"] != "python" ||
				provenance["runtime_version"] != rigPythonVersion || provenance["request_id"] != "request-9" {
				t.Fatalf("provenance = %v", provenance)
			}
		})
	}
}

func TestExecuteWithoutDeclaredOutputsImportsNothing(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.before = writesOutputs(t, map[string]string{"a.txt": "alpha"})
	outcome, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"})
	if err != nil || outcome.Artifacts != nil || len(rig.sink.recorded()) != 0 {
		t.Fatalf("outcome = %+v, err = %v, puts = %d", outcome, err, len(rig.sink.recorded()))
	}
}

func TestExecuteGivesTheProgramAnOutputDirectoryAndRemovesItAfterwards(t *testing.T) {
	rig := newRig(t, nil)
	var mode os.FileMode
	var isDir bool
	rig.stub.before = func(_ context.Context, spec *sandbox.Spec) {
		info, err := os.Stat(filepath.Join(spec.WorkingDir, outputDirName))
		if err != nil {
			t.Error(err)
			return
		}
		mode, isDir = info.Mode().Perm(), info.IsDir()
	}
	if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x", OutputFiles: []string{"a.txt"}}); err != nil {
		t.Fatal(err)
	}
	if !isDir || mode != outputDirMode {
		t.Fatalf("output directory: dir = %v, mode = %v", isDir, mode)
	}
	if left := rig.runDirs(t); len(left) != 0 {
		t.Fatalf("run directories left behind: %v", left)
	}
}

func TestExecuteRefusesBadOutputListsBeforeAnyEffect(t *testing.T) {
	cases := []struct {
		name  string
		files []string
	}{
		{"above the configured count", []string{"a", "b", "c", "d", "e"}},
		{"repeat", []string{"a", "a"}},
		{"parent traversal", []string{"../a"}},
		{"absolute", []string{"/a"}},
		{"bad character", []string{"a b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			_, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x", OutputFiles: tc.files})
			if !hasErrorCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute = %v, want invalid_argument", err)
			}
			if rig.stub.count() != 0 || len(rig.runDirs(t)) != 0 {
				t.Fatalf("a refused request ran %d times and left %v", rig.stub.count(), rig.runDirs(t))
			}
		})
	}
}

func TestArgumentCheckBoundsOutputFiles(t *testing.T) {
	rig := newRig(t, nil)
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"none", `{"runtime":"python","source":"x"}`, true},
		{"at the configured count", `{"runtime":"python","source":"x","output_files":["a","b","c","d"]}`, true},
		{"above the configured count", `{"runtime":"python","source":"x","output_files":["a","b","c","d","e"]}`, false},
		{"traversal", `{"runtime":"python","source":"x","output_files":["a/../b"]}`, false},
		{"repeat", `{"runtime":"python","source":"x","output_files":["a","a"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rig.runner.ArgumentCheck(json.RawMessage(tc.raw))
			if (err == nil) != tc.ok || (err != nil && !hasErrorCode(err, ErrorCodeInvalidArgument)) {
				t.Fatalf("ArgumentCheck = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}

func TestAdapterListsArtifactsBetweenTheDurationAndTheTruncationFlags(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.before = writesOutputs(t, map[string]string{"a.txt": "alpha"})
	request := codeRequest(`{"runtime":"python","source":"x","output_files":["a.txt","gone.txt"]}`)
	request.SessionID, request.RequestID = "session-9", "request-9"
	result, err := rig.runner.Adapter()(t.Context(), request, brokerConstraints())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), `,"artifacts":[{"name":"a.txt","artifact_id":"art-`) ||
		!strings.Contains(string(result.Output), `{"name":"gone.txt","artifact_id":"","digest":"","media_type":"","size":0,"refused":"absent"}],"stdout_truncated":`) {
		t.Fatalf("output = %s", result.Output)
	}
	var decoded encodedResult
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Artifacts) != 2 || decoded.Artifacts[0].MediaType != "text/plain; charset=utf-8" || decoded.Artifacts[0].Size != 5 {
		t.Fatalf("artifacts = %+v", decoded.Artifacts)
	}
}

func TestAdapterKeepsEveryArtifactEntryWhenOutputIsHuge(t *testing.T) {
	rig := newRig(t, func(l *Limits) {
		l.MaxStdoutBytes, l.MaxStderrBytes, l.MaxOutputFiles = 1<<20, 1<<20, tools.MaxCodeOutputFiles
	})
	files := make(map[string]string, tools.MaxCodeOutputFiles)
	names := make([]string, 0, tools.MaxCodeOutputFiles)
	for i := range tools.MaxCodeOutputFiles {
		name := strings.Repeat("n", tools.MaxCodeOutputNameBytes-4) + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ".x"
		files[name] = "content " + name
		names = append(names, name)
	}
	rig.stub.result = sandbox.Result{Stdout: strings.Repeat("o", 1<<20), Stderr: strings.Repeat("e", 1<<20)}
	rig.stub.before = writesOutputs(t, files)
	arguments, err := json.Marshal(map[string]any{"runtime": "python", "source": "x", "output_files": names})
	if err != nil {
		t.Fatal(err)
	}
	request := codeRequest(string(arguments))
	request.SessionID, request.RequestID = "session-9", "request-9"
	result, err := rig.runner.Adapter()(t.Context(), request, brokerConstraints())
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(result.Output)) > defaultMaxResult || !json.Valid(result.Output) {
		t.Fatalf("result is %d bytes, valid = %v", len(result.Output), json.Valid(result.Output))
	}
	var decoded encodedResult
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.StdoutTruncated || !decoded.StderrTruncated || len(decoded.Artifacts) != len(names) {
		t.Fatalf("truncated = %v/%v, artifacts = %d, want %d", decoded.StdoutTruncated, decoded.StderrTruncated, len(decoded.Artifacts), len(names))
	}
	for i, entry := range decoded.Artifacts {
		if entry.Name != names[i] || entry.Refused != "" || entry.ArtifactID == "" {
			t.Fatalf("artifact %d = %+v", i, entry)
		}
	}
}

func TestEncoderFailsInsteadOfDroppingArtifactEntries(t *testing.T) {
	outcome := Outcome{Class: ClassOK, Stdout: "out", Artifacts: []ArtifactEntry{
		{Name: strings.Repeat("a", 64), ArtifactID: "art-0123456789abcdef", Digest: strings.Repeat("d", 64), MediaType: "text/plain", Size: 1},
	}}
	if encoded, err := encodeBounded(&outcome, &Limits{MaxStdoutBytes: 10, MaxStderrBytes: 10}, 200); err == nil {
		t.Fatalf("encoded %d bytes without the full artifact list: %s", len(encoded), encoded)
	}
}
