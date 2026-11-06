package codeexec

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

func TestProbeAcceptsOnlyThePinnedVersion(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		result  sandbox.Result
		err     error
		wantErr func(error) bool
	}{
		{name: "python reports its version", runtime: "python", result: sandbox.Result{Stdout: "3.12.16\n"}},
		{name: "javascript reports its version", runtime: "javascript", result: sandbox.Result{Stdout: "26.13.0\n"}},
		{name: "another patch release", runtime: "python", result: sandbox.Result{Stdout: "3.12.15\n"}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "no output", runtime: "python", wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "non-zero exit", runtime: "python", result: sandbox.Result{Stdout: "3.12.16\n", ExitCode: 1}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "killed by the filter", runtime: "javascript", result: sandbox.Result{Stdout: "26.13.0\n", ExitCode: -1, Signal: int(syscall.SIGSYS)}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "timeout", runtime: "python", result: sandbox.Result{Terminated: true, TimedOut: true, Stdout: "3.12.16\n"}, wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeUnavailable) }},
		{name: "sandbox failure", runtime: "python", err: errors.New("no cgroup"), wantErr: func(err error) bool { return hasErrorCode(err, ErrorCodeRunFailed) }},
		{name: "go has no probe", runtime: "go", wantErr: func(err error) bool { return errors.Is(err, interpreter.ErrProbeUnsupported) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.result, rig.stub.err = tc.result, tc.err
			bundle, err := rig.store.Bundle(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			err = rig.runner.Probe(t.Context(), &bundle)
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

func TestProbeProgramsPrintOnlyTheVersion(t *testing.T) {
	for _, runtime := range []string{"python", "javascript"} {
		language, _ := languageFor(runtime)
		if strings.Count(language.probe, "\n") > 2 {
			t.Errorf("the %s probe is longer than a version print: %q", runtime, language.probe)
		}
	}
}
