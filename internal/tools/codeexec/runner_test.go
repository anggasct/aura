package codeexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

func hasErrorCode(err error, want ErrorCode) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}

func TestNewRunnerRejectsIncompleteOptions(t *testing.T) {
	rig := newRig(t, nil)
	valid := Options{Store: rig.store, Verified: rig.set, Artifacts: rig.sink, RunRoot: rig.runRoot, Limits: rigLimits()}
	cases := []struct {
		name   string
		mutate func(*Options)
		nilOpt bool
	}{
		{name: "nil options", nilOpt: true},
		{name: "missing store", mutate: func(o *Options) { o.Store = nil }},
		{name: "missing verified set", mutate: func(o *Options) { o.Verified = nil }},
		{name: "missing artifact sink", mutate: func(o *Options) { o.Artifacts = nil }},
		{name: "relative run root", mutate: func(o *Options) { o.RunRoot = "runs" }},
		{name: "empty run root", mutate: func(o *Options) { o.RunRoot = "" }},
		{name: "zero timeout", mutate: func(o *Options) { o.Limits.Timeout = 0 }},
		{name: "timeout below a second", mutate: func(o *Options) { o.Limits.Timeout = time.Second - 1 }},
		{name: "zero source limit", mutate: func(o *Options) { o.Limits.MaxSourceBytes = 0 }},
		{name: "zero stdin limit", mutate: func(o *Options) { o.Limits.MaxStdinBytes = 0 }},
		{name: "zero stdout limit", mutate: func(o *Options) { o.Limits.MaxStdoutBytes = 0 }},
		{name: "zero stderr limit", mutate: func(o *Options) { o.Limits.MaxStderrBytes = 0 }},
		{name: "zero memory limit", mutate: func(o *Options) { o.Limits.MaxMemoryBytes = 0 }},
		{name: "zero process limit", mutate: func(o *Options) { o.Limits.MaxProcesses = 0 }},
		{name: "zero run directory limit", mutate: func(o *Options) { o.Limits.MaxRunDirBytes = 0 }},
		{name: "zero file limit", mutate: func(o *Options) { o.Limits.MaxFileBytes = 0 }},
		{name: "zero output file limit", mutate: func(o *Options) { o.Limits.MaxOutputFiles = 0 }},
		{name: "output file limit above the tool maximum", mutate: func(o *Options) { o.Limits.MaxOutputFiles = tools.MaxCodeOutputFiles + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := valid
			var arg *Options
			if !tc.nilOpt {
				tc.mutate(&options)
				arg = &options
			}
			runner, err := NewRunner(arg)
			if !hasErrorCode(err, ErrorCodeInvalidArgument) || runner != nil {
				t.Fatalf("NewRunner = %v, %v; want invalid_argument and no runner", runner, err)
			}
		})
	}
	if _, err := NewRunner(&valid); err != nil {
		t.Fatalf("NewRunner with valid options: %v", err)
	}
}

func TestExecuteBuildsTheInterpreterSpec(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
		env     []string
	}{
		{"python", "print(1)", nil},
		{"javascript", "console.log(1)", []string{"OPENSSL_CONF=/dev/null"}},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			rig := newRig(t, nil)
			outcome, err := rig.runner.Execute(t.Context(), &Request{Runtime: tc.runtime, Source: tc.source, Stdin: "input"})
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Class != ClassOK {
				t.Fatalf("outcome = %+v", outcome)
			}
			call := rig.stub.last(t)
			bundle, err := rig.store.Bundle(tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			spec := call.spec
			if spec.Profile != sandbox.ProfileInterpreter || spec.AllowNetwork {
				t.Fatalf("profile = %q, network = %v", spec.Profile, spec.AllowNetwork)
			}
			if filepath.Dir(spec.WorkingDir) != rig.runRoot || !strings.HasPrefix(filepath.Base(spec.WorkingDir), runDirPrefix) {
				t.Fatalf("working dir %q is not a run directory under %q", spec.WorkingDir, rig.runRoot)
			}
			if !slices.Equal(spec.ReadOnlyPaths, []string{bundle.Dir}) || len(spec.ReadWritePaths) != 0 {
				t.Fatalf("paths = %v / %v, want only the bundle read-only", spec.ReadOnlyPaths, spec.ReadWritePaths)
			}
			if string(spec.Stdin) != "input" {
				t.Fatalf("stdin = %q", spec.Stdin)
			}
			wantEnv := append([]string{"HOME=" + spec.WorkingDir, "TMPDIR=" + spec.WorkingDir, "LANG=C.UTF-8"}, tc.env...)
			if !slices.Equal(spec.AllowEnv, wantEnv) {
				t.Fatalf("environment = %v, want %v", spec.AllowEnv, wantEnv)
			}
			wantLimits := sandbox.Limits{
				MemoryBytes: 256 << 20, CPUTime: 10 * time.Second, MaxOutputBytes: 1 << 10, MaxOpenFiles: openFilesLimit,
				MaxProcesses: 32, FileBytes: 4 << 20, Timeout: 10*time.Second + wallClockMargin, NoProcessRlimit: true, RunDirBytes: 8 << 20,
			}
			if spec.Limits != wantLimits {
				t.Fatalf("limits = %+v, want %+v", spec.Limits, wantLimits)
			}
			if call.command != bundle.Executable || !slices.Equal(call.args, []string{"-flag", call.sourceName}) {
				t.Fatalf("command = %s %v", call.command, call.args)
			}
			if call.source != tc.source || call.sourceMode != sourceFileMode {
				t.Fatalf("source file = %q with mode %v", call.source, call.sourceMode)
			}
		})
	}
}

func TestExecuteNamesTheSourceFilePerLanguage(t *testing.T) {
	rig := newRig(t, nil)
	for runtime, want := range map[string]string{"python": "main.py", "javascript": "main.js"} {
		if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: runtime, Source: "x"}); err != nil {
			t.Fatal(err)
		}
		if got := rig.stub.last(t).sourceName; got != want {
			t.Errorf("%s source file = %q, want %q", runtime, got, want)
		}
	}
}

func TestExecuteHonoursALowerRequestTimeout(t *testing.T) {
	cases := []struct {
		name    string
		request time.Duration
		want    time.Duration
	}{
		{"unset", 0, 10 * time.Second},
		{"lower", 3 * time.Second, 3 * time.Second},
		{"below a second is raised", 200 * time.Millisecond, time.Second},
		{"higher than the configured one is ignored", time.Minute, 10 * time.Second},
		{"negative is ignored", -time.Second, 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x", Timeout: tc.request}); err != nil {
				t.Fatal(err)
			}
			limits := rig.stub.last(t).spec.Limits
			if limits.CPUTime != tc.want || limits.Timeout != tc.want+wallClockMargin {
				t.Fatalf("cpu %v and wall %v, want %v and %v", limits.CPUTime, limits.Timeout, tc.want, tc.want+wallClockMargin)
			}
		})
	}
}

func TestExecuteGivesTheWallClockMoreRoomThanTheCPUBudget(t *testing.T) {
	rig := newRig(t, nil)
	if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	limits := rig.stub.last(t).spec.Limits
	if limits.Timeout < limits.CPUTime+2*time.Second-time.Nanosecond {
		t.Fatalf("wall clock %v leaves no room for the CPU hard limit above %v", limits.Timeout, limits.CPUTime)
	}
}

func TestExecuteRefusesWithoutStartingAProcess(t *testing.T) {
	cases := []struct {
		name     string
		request  Request
		runtimes []rigRuntime
		want     ErrorCode
	}{
		{"go is not wired in this step", Request{Runtime: "go", Source: "package main"}, nil, ErrorCodeUnavailable},
		{"unknown runtime", Request{Runtime: "ruby", Source: "puts 1"}, nil, ErrorCodeUnavailable},
		{"empty runtime", Request{Source: "x"}, nil, ErrorCodeUnavailable},
		{"runtime that is not installed", Request{Runtime: "javascript", Source: "x"}, []rigRuntime{
			{id: "python", version: rigPythonVersion, install: true}, {id: "javascript", version: "26.13.0"},
		}, ErrorCodeUnavailable},
		{"oversized source", Request{Runtime: "python", Source: strings.Repeat("x", 1<<10+1)}, nil, ErrorCodeInvalidArgument},
		{"oversized stdin", Request{Runtime: "python", Source: "x", Stdin: strings.Repeat("x", 1<<10+1)}, nil, ErrorCodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			if tc.runtimes != nil {
				rig = newRigWith(t, nil, tc.runtimes)
			}
			_, err := rig.runner.Execute(t.Context(), &tc.request)
			if !hasErrorCode(err, tc.want) {
				t.Fatalf("Execute = %v, want %s", err, tc.want)
			}
			if rig.stub.count() != 0 || len(rig.runDirs(t)) != 0 {
				t.Fatalf("a refused request started %d sandbox runs and left %v", rig.stub.count(), rig.runDirs(t))
			}
		})
	}
	t.Run("nil request", func(t *testing.T) {
		rig := newRig(t, nil)
		if _, err := rig.runner.Execute(t.Context(), nil); !hasErrorCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("Execute(nil) = %v", err)
		}
	})
	t.Run("the limits sit exactly at the boundary", func(t *testing.T) {
		rig := newRig(t, nil)
		request := Request{Runtime: "python", Source: strings.Repeat("x", 1<<10), Stdin: strings.Repeat("x", 1<<10)}
		if _, err := rig.runner.Execute(t.Context(), &request); err != nil {
			t.Fatalf("Execute at the limits: %v", err)
		}
	})
}

func TestExecuteNeverSearchesThePath(t *testing.T) {
	rig := newRig(t, nil)
	if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	call := rig.stub.last(t)
	if !filepath.IsAbs(call.command) || !strings.HasPrefix(call.command, rig.data) {
		t.Fatalf("command %q is not an absolute path inside the installed bundle", call.command)
	}
}

func TestExecuteRemovesTheRunDirectoryOnEveryPath(t *testing.T) {
	cases := []struct {
		name   string
		result sandbox.Result
		err    error
		cancel bool
	}{
		{name: "success", result: sandbox.Result{}},
		{name: "non-zero exit", result: sandbox.Result{ExitCode: 2}},
		{name: "crash", result: sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGSEGV)}},
		{name: "timeout", result: sandbox.Result{Terminated: true, TimedOut: true}},
		{name: "cancellation", result: sandbox.Result{Terminated: true}, cancel: true},
		{name: "sandbox failure", err: sandbox.Errorf(sandbox.ErrorCodeSandboxInitFailed, "init failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.result, rig.stub.err = tc.result, tc.err
			var seen string
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rig.stub.before = func(_ context.Context, spec *sandbox.Spec) {
				seen = spec.WorkingDir
				if info, err := os.Stat(seen); err != nil || !info.IsDir() || info.Mode().Perm() != runDirMode {
					t.Errorf("the run directory is not a private directory: %v %v", info, err)
				}
				if tc.cancel {
					cancel()
				}
			}
			_, _ = rig.runner.Execute(ctx, &Request{Runtime: "python", Source: "x"})
			if seen == "" {
				t.Fatal("the sandbox never ran")
			}
			if left := rig.runDirs(t); len(left) != 0 {
				t.Fatalf("run directories left behind: %v", left)
			}
		})
	}
}

func TestExecuteReportsTheOutcome(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.result = sandbox.Result{
		ExitCode: 3, Stdout: "out", Stderr: "err", StdoutTruncated: true, StderrTruncated: true,
	}
	outcome, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := Outcome{Class: ClassExitNonzero, ExitCode: 3, Stdout: "out", Stderr: "err", StdoutTruncated: true, StderrTruncated: true}
	outcome.Duration = 0
	if !reflect.DeepEqual(outcome, want) {
		t.Fatalf("outcome = %+v, want %+v", outcome, want)
	}
}

func TestExecuteClassifiesWithTheCPUBudgetOfTheRun(t *testing.T) {
	rig := newRig(t, nil)
	rig.stub.result = sandbox.Result{ExitCode: -1, Signal: int(syscall.SIGKILL), CPUTime: 4 * time.Second}
	outcome, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Class != ClassCPULimit {
		t.Fatalf("class = %s, want %s for 4s of CPU against a 3s budget", outcome.Class, ClassCPULimit)
	}
}

func TestExecuteWrapsSandboxFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"coded", sandbox.Errorf(sandbox.ErrorCodeSandboxInitFailed, "init failed")},
		{"uncoded", errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, nil)
			rig.stub.err = tc.err
			_, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "SECRET-SOURCE", Stdin: "SECRET-STDIN"})
			if !hasErrorCode(err, ErrorCodeRunFailed) || !errors.Is(err, tc.err) {
				t.Fatalf("Execute = %v, want run_failed wrapping the cause", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("the error leaks program data: %v", err)
			}
		})
	}
}

func TestExecuteBoundsConcurrentRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig := newRig(t, nil)
		release := make(chan struct{})
		rig.stub.before = func(_ context.Context, _ *sandbox.Spec) { <-release }
		var group sync.WaitGroup
		for range maxConcurrentRuns {
			group.Go(func() {
				if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"}); err != nil {
					t.Errorf("Execute: %v", err)
				}
			})
		}
		synctest.Wait()
		if got := rig.stub.count(); got != maxConcurrentRuns {
			t.Fatalf("%d runs started, want %d", got, maxConcurrentRuns)
		}
		ctx, cancel := context.WithCancel(t.Context())
		waiting := make(chan error, 1)
		go func() {
			_, err := rig.runner.Execute(ctx, &Request{Runtime: "python", Source: "x"})
			waiting <- err
		}()
		synctest.Wait()
		if got := rig.stub.count(); got != maxConcurrentRuns {
			t.Fatalf("a run beyond the bound started: %d runs", got)
		}
		cancel()
		if err := <-waiting; !hasErrorCode(err, ErrorCodeRunFailed) || !errors.Is(err, context.Canceled) {
			t.Fatalf("a queued run that was cancelled = %v, want run_failed wrapping the cancellation", err)
		}
		if got := rig.stub.count(); got != maxConcurrentRuns {
			t.Fatalf("a cancelled queued run started: %d runs", got)
		}
		close(release)
		group.Wait()
		if left := rig.runDirs(t); len(left) != 0 {
			t.Fatalf("run directories left behind: %v", left)
		}
	})
}

func TestExecuteFreesTheSlotAfterEachRun(t *testing.T) {
	rig := newRig(t, nil)
	for range 3 * maxConcurrentRuns {
		if _, err := rig.runner.Execute(t.Context(), &Request{Runtime: "python", Source: "x"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLanguageTable(t *testing.T) {
	cases := []struct {
		runtime string
		source  string
		known   bool
	}{
		{"python", "main.py", true},
		{"javascript", "main.js", true},
		{"go", "", false},
		{"", "", false},
		{"Python", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			language, known := languageFor(tc.runtime)
			if known != tc.known || language.source != tc.source {
				t.Fatalf("languageFor = %+v, %v", language, known)
			}
			if known && language.probe == "" {
				t.Fatal("a supported language needs a version probe")
			}
		})
	}
	for version, want := range map[string]string{"3.12.16+20261026": "3.12.16", "26.13.0": "26.13.0", "1.0.0+a+b": "1.0.0"} {
		if got := coreVersion(version); got != want {
			t.Errorf("coreVersion(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestRunnerIsAProber(t *testing.T) {
	var _ interpreter.Prober = (*Runner)(nil)
}
