package codeexec

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/sandbox"
)

func TestProbeAcceptsOnlyThePinnedVersion(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		result  sandbox.Result
		err     error
		wantErr func(error) bool
		unknown bool
	}{
		{name: "python reports its version", runtime: "python", result: sandbox.Result{Stdout: "3.12.16\n"}},
		{name: "javascript reports its version", runtime: "javascript", result: sandbox.Result{Stdout: "26.13.0\n"}},
		{name: "another patch release", runtime: "python", result: sandbox.Result{Stdout: "3.12.15\n"}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "no output", runtime: "python", wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "non-zero exit", runtime: "python", result: sandbox.Result{Stdout: "3.12.16\n", ExitCode: 1}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "killed by the filter", runtime: "javascript", result: sandbox.Result{Stdout: "26.13.0\n", ExitCode: -1, Signal: int(syscall.SIGSYS)}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "timeout", runtime: "python", result: sandbox.Result{Terminated: true, TimedOut: true, Stdout: "3.12.16\n"}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "sandbox failure", runtime: "python", err: errors.New("no cgroup"), wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeRunFailed) }},
		{name: "go reports its version", runtime: "go", result: sandbox.Result{Stdout: "1.27.3\n"}},
		{name: "go another patch release", runtime: "go", result: sandbox.Result{Stdout: "1.27.2\n"}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "go build failure", runtime: "go", result: sandbox.Result{ExitCode: 1, Stderr: "compile failed"}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "unknown runtime has no probe", runtime: "ruby", unknown: true, wantErr: func(err error) bool { return errors.Is(err, interpreter.ErrProbeUnsupported) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.result, rig.stub.err = tc.result, tc.err
			bundle := interpreter.Bundle{Runtime: interpreter.Runtime{ID: tc.runtime}}
			if !tc.unknown {
				var err error
				if bundle, err = rig.store.Bundle(tc.runtime); err != nil {
					t.Fatal(err)
				}
			}
			err := rig.runner.Probe(t.Context(), &bundle)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Probe = %v", err)
			}
			if tc.wantErr != nil && (err == nil || !tc.wantErr(err)) {
				t.Fatalf("Probe = %v", err)
			}
		})
	}
}

func TestProbeRunsTheLanguageProbeInsideTheInterpreterProfile(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.result = sandbox.Result{Stdout: "3.12.16\n"}
	bundle, err := rig.store.Bundle("python")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.runner.Probe(t.Context(), &bundle); err != nil {
		t.Fatal(err)
	}
	call := rig.stub.last(t)
	language, _ := languageFor("python")
	if call.source != language.probe || call.spec.Profile != sandbox.ProfileInterpreter {
		t.Fatalf("probe ran %q under %q", call.source, call.spec.Profile)
	}
	want := sandbox.Limits{
		MemoryBytes: probeMemoryBytes, CPUTime: probeTimeout, MaxOutputBytes: probeOutputBytes, MaxOpenFiles: openFilesLimit,
		MaxProcesses: probeProcesses, FileBytes: probeRunDirBytes, Timeout: probeTimeout, NoProcessRlimit: true, RunDirBytes: probeRunDirBytes,
	}
	if call.spec.Limits != want {
		t.Fatalf("probe limits = %+v, want %+v", call.spec.Limits, want)
	}
	if left := rig.runDirs(t); len(left) != 0 {
		t.Fatalf("the probe left run directories: %v", left)
	}
}

func TestProbeBuildsAndRunsTheGoProbeInTwoPhases(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.script = []sandbox.Result{{Stdout: "build noise"}, {Stdout: "1.27.3\n"}}
	bundle, err := rig.store.Bundle("go")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.runner.Probe(t.Context(), &bundle); err != nil {
		t.Fatal(err)
	}
	if rig.stub.count() != 2 {
		t.Fatalf("the probe made %d sandbox runs, want a build and a run", rig.stub.count())
	}
	build, run := rig.stub.call(t, 0), rig.stub.call(t, 1)
	if build.spec.Limits.Timeout != rigLimits().BuildTimeout || build.spec.Limits.MemoryBytes != buildMemoryBytes {
		t.Fatalf("probe build limits = %+v", build.spec.Limits)
	}
	want := sandbox.Limits{
		MemoryBytes: probeMemoryBytes, CPUTime: probeTimeout, MaxOutputBytes: probeOutputBytes, MaxOpenFiles: openFilesLimit,
		MaxProcesses: probeProcesses, FileBytes: probeRunDirBytes, Timeout: probeTimeout, NoProcessRlimit: true, RunDirBytes: probeRunDirBytes,
	}
	if run.spec.Limits != want {
		t.Fatalf("probe run limits = %+v, want %+v", run.spec.Limits, want)
	}
}

func TestProbeStopsAtAFailedGoBuild(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.script = []sandbox.Result{{ExitCode: 1, Stderr: "compile failed"}, {Stdout: "1.27.3\n"}}
	bundle, err := rig.store.Bundle("go")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.runner.Probe(t.Context(), &bundle); !hasErrorCode(err, ErrorCodeUnavailable) {
		t.Fatalf("Probe = %v, want %s", err, ErrorCodeUnavailable)
	}
	if rig.stub.count() != 1 {
		t.Fatalf("a failed build must not start the program: %d runs", rig.stub.count())
	}
}

func TestProbeProgramsPrintOnlyTheVersion(t *testing.T) {
	lines := map[string]int{"python": 2, "javascript": 2, "go": 3}
	for runtime, most := range lines {
		language, _ := languageFor(runtime)
		if strings.Count(language.probe, "\n") > most {
			t.Errorf("the %s probe is longer than a version print: %q", runtime, language.probe)
		}
	}
}
