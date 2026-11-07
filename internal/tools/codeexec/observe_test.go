package codeexec

import (
	"context"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools"
)

func observingRig(t *testing.T) (*rig, *observations) {
	t.Helper()
	rig := newRig(t, nil)
	seen := &observations{}
	rig.runner.observer = func(_ context.Context, observation *Observation) { seen.add(*observation) }
	return rig, seen
}

func TestObservationLabelsComeFromClosedSets(t *testing.T) {
	classes := []ResultClass{
		ClassOK, ClassExitNonzero, ClassTimeout, ClassCancelled, ClassCPULimit, ClassMemoryLimit,
		ClassProcessLimit, ClassFileLimit, ClassDiskLimit, ClassPolicyDenied, ClassRuntimeCrashed,
	}
	cases := []struct {
		name   string
		result sandbox.Result
		want   ResultClass
	}{
		{"ok", sandbox.Result{}, ClassOK},
		{"exit nonzero", sandbox.Result{ExitCode: 2}, ClassExitNonzero},
		{"timeout", sandbox.Result{Terminated: true, TimedOut: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassTimeout},
		{"cancelled", sandbox.Result{Terminated: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassCancelled},
		{"policy denied", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSYS)}, ClassPolicyDenied},
		{"memory limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), OOMKilled: true}, ClassMemoryLimit},
		{"disk limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), DiskLimited: true}, ClassDiskLimit},
		{"file limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGXFSZ), FileLimited: true}, ClassFileLimit},
		{"process limit", sandbox.Result{ExitCode: 1, PidsLimited: true}, ClassProcessLimit},
		{"runtime crash", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSEGV)}, ClassRuntimeCrashed},
	}
	for _, runtime := range tools.CodeRuntimes() {
		for _, tc := range cases {
			t.Run(runtime+"/"+tc.name, func(t *testing.T) {
				rig, seen := observingRig(t)
				rig.stub.result = tc.result
				if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: runtime, Source: "x"}); err != nil {
					t.Fatal(err)
				}
				observed := seen.all()
				if len(observed) != 1 {
					t.Fatalf("observations = %d, want 1", len(observed))
				}
				got := observed[0]
				if got.Class != tc.want || !slices.Contains(classes, got.Class) {
					t.Errorf("class = %q, want %q from the closed set", got.Class, tc.want)
				}
				if got.Runtime != runtime || !slices.Contains(tools.CodeRuntimes(), got.Runtime) {
					t.Errorf("runtime = %q, want %q", got.Runtime, runtime)
				}
				wantPhase := PhaseRun
				if runtime == "go" && tc.want != ClassOK {
					wantPhase = PhaseBuild
				}
				if got.Phase != wantPhase || got.Compiled != (runtime == "go") {
					t.Errorf("phase = %q, compiled = %v; want %q", got.Phase, got.Compiled, wantPhase)
				}
			})
		}
	}
}

func TestObservationHoldsNoFreeFormText(t *testing.T) {
	allowed := []string{"Runtime", "Class", "Phase"}
	observation := reflect.TypeFor[Observation]()
	for i := range observation.NumField() {
		field := observation.Field(i)
		switch field.Type.Kind() {
		case reflect.String:
			if !slices.Contains(allowed, field.Name) {
				t.Errorf("field %s can carry free-form text", field.Name)
			}
		case reflect.Slice, reflect.Map, reflect.Interface, reflect.Pointer:
			t.Errorf("field %s can carry arbitrary content", field.Name)
		default:
		}
	}
}

func TestOnlyFinishedRunsAreObserved(t *testing.T) {
	t.Run("unknown runtime", func(t *testing.T) {
		rig, seen := observingRig(t)
		if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "ruby", Source: "x"}); err == nil {
			t.Fatal("an unknown runtime ran")
		}
		if got := len(seen.all()); got != 0 {
			t.Fatalf("observations = %d, want none", got)
		}
	})
	t.Run("sandbox refusal", func(t *testing.T) {
		rig, seen := observingRig(t)
		rig.stub.err = sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "no sandbox")
		if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"}); err == nil {
			t.Fatal("a refused sandbox produced an outcome")
		}
		if got := len(seen.all()); got != 0 {
			t.Fatalf("observations = %d, want none", got)
		}
	})
	t.Run("version probe", func(t *testing.T) {
		rig, seen := observingRig(t)
		bundle, err := rig.store.Bundle("python")
		if err != nil {
			t.Fatal(err)
		}
		rig.stub.result = sandbox.Result{Stdout: coreVersion(bundle.Runtime.Version)}
		if err := rig.runner.Probe(t.Context(), &bundle); err != nil {
			t.Fatal(err)
		}
		if got := len(seen.all()); got != 0 {
			t.Fatalf("a probe was recorded as a user run: %d observations", got)
		}
	})
}

func TestObservationCountsOutputAndArtifacts(t *testing.T) {
	rig, seen := observingRig(t)
	rig.stub.result = sandbox.Result{Stdout: "12345", Stderr: "678"}
	rig.stub.before = writesOutputs(t, map[string]string{"a.txt": "alpha", "b.txt": "bb"})
	if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x", OutputFiles: []string{"a.txt", "b.txt", "c.txt"}}); err != nil {
		t.Fatal(err)
	}
	got := seen.all()[0]
	if got.StdoutBytes != 5 || got.StderrBytes != 3 || got.ArtifactBytes != 7 || got.Imported != 2 || got.Refused != 1 {
		t.Fatalf("observation = %+v", got)
	}
}
