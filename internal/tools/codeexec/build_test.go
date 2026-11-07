package codeexec

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
)

func goRequest(source string) *Request {
	return &Request{Runtime: "go", Source: source, Stdin: "input"}
}

func TestGoBuildPhaseBuildsInsideItsOwnLimits(t *testing.T) {
	rig := newRig(t, func(l *Limits) { l.MaxFileBytes = 1 << 20; l.MaxRunDirBytes = 8 << 20 })
	if _, err := rig.runner.Execute(t.Context(), goRequest("package main")); err != nil {
		t.Fatal(err)
	}
	if rig.stub.count() != 2 {
		t.Fatalf("a go call made %d sandbox runs, want a build and a run", rig.stub.count())
	}
	bundle, err := rig.store.Bundle("go")
	if err != nil {
		t.Fatal(err)
	}
	build := rig.stub.call(t, 0)
	program := filepath.Join(build.spec.WorkingDir, ".go", "program")
	if build.command != bundle.Executable || !slices.Equal(build.args, []string{"-build", "-o", program, "main.go"}) {
		t.Fatalf("build command = %s %v", build.command, build.args)
	}
	wantLimits := sandbox.Limits{
		MemoryBytes: buildMemoryBytes, CPUTime: 20 * time.Second, MaxOutputBytes: 1 << 10, MaxOpenFiles: openFilesLimit,
		MaxProcesses: buildProcesses, FileBytes: buildFileBytes, Timeout: 20 * time.Second, NoProcessRlimit: true, RunDirBytes: buildRunDirBytes,
	}
	if build.spec.Limits != wantLimits {
		t.Fatalf("build limits = %+v, want %+v\nthe build must not depend on the artifact or run directory limits", build.spec.Limits, wantLimits)
	}
	if build.spec.Profile != sandbox.ProfileInterpreter || build.spec.AllowNetwork || len(build.spec.Stdin) != 0 {
		t.Fatalf("build spec = %+v", build.spec)
	}
	if !slices.Equal(build.spec.ReadOnlyPaths, []string{bundle.Dir}) {
		t.Fatalf("build paths = %v, want only the bundle", build.spec.ReadOnlyPaths)
	}
	if build.source != "package main" {
		t.Fatalf("build source = %q", build.source)
	}
}

func TestGoRunPhaseRunsTheBuiltBinaryWithAMinimalEnvironment(t *testing.T) {
	rig := newRig(t, nil)
	if _, err := rig.runner.Execute(t.Context(), goRequest("package main")); err != nil {
		t.Fatal(err)
	}
	run := rig.stub.call(t, 1)
	program := filepath.Join(run.spec.WorkingDir, ".go", "program")
	if run.command != program || !slices.Equal(run.args, []string{"-run"}) {
		t.Fatalf("run command = %s %v, want the built binary", run.command, run.args)
	}
	if want := []string{"HOME=" + run.spec.WorkingDir, "TMPDIR=" + run.spec.WorkingDir, "LANG=C.UTF-8"}; !slices.Equal(run.spec.AllowEnv, want) {
		t.Fatalf("run environment = %v, want %v", run.spec.AllowEnv, want)
	}
	if len(run.spec.ReadOnlyPaths) != 0 {
		t.Fatalf("the program must not read the toolchain bundle: %v", run.spec.ReadOnlyPaths)
	}
	if string(run.spec.Stdin) != "input" {
		t.Fatalf("run stdin = %q", run.spec.Stdin)
	}
	wantLimits := sandbox.Limits{
		MemoryBytes: 256 << 20, CPUTime: 10 * time.Second, MaxOutputBytes: 1 << 10, MaxOpenFiles: openFilesLimit,
		MaxProcesses: 32, FileBytes: 4 << 20, Timeout: 10*time.Second + wallClockMargin, NoProcessRlimit: true, RunDirBytes: 8 << 20,
	}
	if run.spec.Limits != wantLimits {
		t.Fatalf("run limits = %+v, want %+v", run.spec.Limits, wantLimits)
	}
	build := rig.stub.call(t, 0)
	if len(build.spec.Stdin) != 0 {
		t.Fatalf("the build read stdin %q", build.spec.Stdin)
	}
}

func TestGoRunTimeoutAppliesToTheProgramOnly(t *testing.T) {
	rig := newRig(t, nil)
	request := goRequest("package main")
	request.Timeout = 3 * time.Second
	if _, err := rig.runner.Execute(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	build, run := rig.stub.call(t, 0).spec.Limits, rig.stub.call(t, 1).spec.Limits
	if build.Timeout != rigLimits().BuildTimeout || build.CPUTime != rigLimits().BuildTimeout {
		t.Fatalf("build limits followed the request timeout: %+v", build)
	}
	if run.CPUTime != 3*time.Second || run.Timeout != 3*time.Second+wallClockMargin {
		t.Fatalf("run limits = %+v, want the request timeout", run)
	}
}

func TestGoBuildScratchIsRemovedBeforeTheProgramStarts(t *testing.T) {
	rig := newRig(t, nil)
	var seen []string
	rig.stub.before = func(_ context.Context, spec *sandbox.Spec) {
		root := spec.WorkingDir
		if len(seen) == 0 {
			seen = append(seen, "build")
			for _, file := range []string{".go/cache/entry", ".go/path/mod", ".go/tmp/go-build/obj", ".go/program"} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0o700); err != nil {
					t.Error(err)
				}
				if err := os.WriteFile(filepath.Join(root, file), []byte("x"), 0o600); err != nil {
					t.Error(err)
				}
			}
			return
		}
		entries, err := os.ReadDir(filepath.Join(root, ".go"))
		if err != nil {
			t.Error(err)
		}
		for _, entry := range entries {
			seen = append(seen, entry.Name())
		}
	}
	if _, err := rig.runner.Execute(t.Context(), goRequest("package main")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"build", "program"}) {
		t.Fatalf("the program saw %v in its toolchain directory, want only the binary", seen[1:])
	}
}

func TestGoBuildFailureSkipsTheProgramAndShowsTheCompilerOutput(t *testing.T) {
	cases := []struct {
		name   string
		result sandbox.Result
		want   ResultClass
	}{
		{"compile error", sandbox.Result{ExitCode: 1, Stderr: "main.go:2: undefined: x"}, ClassExitNonzero},
		{"build timeout", sandbox.Result{Terminated: true, TimedOut: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassTimeout},
		{"build out of memory", sandbox.Result{OOMKilled: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassMemoryLimit},
		{"build disk limit", sandbox.Result{Terminated: true, DiskLimited: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassDiskLimit},
		{"build killed by the filter", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSYS)}, ClassPolicyDenied},
		{"build cancelled", sandbox.Result{Terminated: true, ExitCode: -1, Signal: int(syscall.SIGKILL)}, ClassCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			tc.result.Stdout = "build stdout"
			rig.stub.script = []sandbox.Result{tc.result, {Stdout: "must not run"}}
			outcome, err := rig.runner.Execute(t.Context(), goRequest("package main"))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Class != tc.want || outcome.ExitCode != tc.result.ExitCode || outcome.Signal != tc.result.Signal {
				t.Fatalf("outcome = %+v, want class %s", outcome, tc.want)
			}
			if outcome.Stderr != tc.result.Stderr || outcome.Stdout != "build stdout" || outcome.Artifacts != nil {
				t.Fatalf("a failed build must report its own output and import nothing: %+v", outcome)
			}
			if rig.stub.count() != 1 {
				t.Fatalf("a failed build started the program: %d runs", rig.stub.count())
			}
			if left := rig.runDirs(t); len(left) != 0 {
				t.Fatalf("run directories left behind: %v", left)
			}
		})
	}
}

func TestGoBuildOutputIsNotMixedIntoTheProgramOutput(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.script = []sandbox.Result{
		{Stdout: "build stdout", Stderr: "go: downloading nothing"},
		{Stdout: "program stdout", Stderr: "program stderr", ExitCode: 3},
	}
	outcome, err := rig.runner.Execute(t.Context(), goRequest("package main"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Class != ClassExitNonzero || outcome.ExitCode != 3 || outcome.Stdout != "program stdout" || outcome.Stderr != "program stderr" {
		t.Fatalf("outcome = %+v, want the program's own result", outcome)
	}
}

func TestGoProgramClassesComeStraightFromTheSandbox(t *testing.T) {
	cases := []struct {
		name   string
		result sandbox.Result
		want   ResultClass
	}{
		{"filter kill", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSYS)}, ClassPolicyDenied},
		{"cpu limit", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), CPUTime: 10 * time.Second}, ClassCPULimit},
		{"crash", sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSEGV)}, ClassRuntimeCrashed},
		{"exit code", sandbox.Result{ExitCode: 7}, ClassExitNonzero},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.script = []sandbox.Result{{}, tc.result}
			outcome, err := rig.runner.Execute(t.Context(), goRequest("package main"))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Class != tc.want || outcome.ExitCode != tc.result.ExitCode || outcome.Signal != tc.result.Signal {
				t.Fatalf("outcome = %+v, want class %s with the sandbox's exit code and signal", outcome, tc.want)
			}
		})
	}
}

func TestGoCancelBetweenThePhasesIsCancelled(t *testing.T) {
	rig := newRig(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rig.stub.before = func(context.Context, *sandbox.Spec) { cancel() }
	outcome, err := rig.runner.Execute(ctx, goRequest("package main"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Class != ClassCancelled || outcome.Signal != int(syscall.SIGKILL) {
		t.Fatalf("outcome = %+v", outcome)
	}
	if rig.stub.count() != 1 {
		t.Fatalf("a cancelled call started the program: %d runs", rig.stub.count())
	}
	if left := rig.runDirs(t); len(left) != 0 {
		t.Fatalf("run directories left behind: %v", left)
	}
}

func TestGoDurationCountsTheBuildAndTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig := newRig(t, nil)
		delays := []time.Duration{3 * time.Second, 4 * time.Second}
		rig.stub.before = func(context.Context, *sandbox.Spec) {
			time.Sleep(delays[0])
			delays = delays[1:]
		}
		outcome, err := rig.runner.Execute(t.Context(), goRequest("package main"))
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Duration != 7*time.Second {
			t.Fatalf("duration = %v, want the build and the run together", outcome.Duration)
		}
	})
}

func TestSingleStageRuntimesRunOnce(t *testing.T) {
	for _, runtime := range []string{"python", "javascript"} {
		t.Run(runtime, func(t *testing.T) {
			rig := newRig(t, nil)
			if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: runtime, Source: "x"}); err != nil {
				t.Fatal(err)
			}
			if rig.stub.count() != 1 {
				t.Fatalf("%s made %d sandbox runs", runtime, rig.stub.count())
			}
			bundle, _ := rig.store.Bundle(runtime)
			if !slices.Equal(rig.stub.last(t).spec.ReadOnlyPaths, []string{bundle.Dir}) {
				t.Fatalf("%s lost its bundle access", runtime)
			}
			for _, entry := range rig.stub.last(t).spec.AllowEnv {
				if strings.HasPrefix(entry, "GO") {
					t.Fatalf("%s environment carries %s", runtime, entry)
				}
			}
		})
	}
}
