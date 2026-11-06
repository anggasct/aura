package codeexec

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

const (
	maxConcurrentRuns = 4
	runDirPrefix      = "run-"
	runDirMode        = 0o700
	sourceFileMode    = 0o600
	openFilesLimit    = 1024
	wallClockMargin   = 2 * time.Second
	probeTimeout      = 30 * time.Second
	probeMemoryBytes  = 512 << 20
	probeProcesses    = 64
	probeRunDirBytes  = 16 << 20
	probeOutputBytes  = 4 << 10
)

type Limits struct {
	Timeout        time.Duration
	MaxSourceBytes int64
	MaxStdinBytes  int64
	MaxStdoutBytes int64
	MaxStderrBytes int64
	MaxMemoryBytes int64
	MaxProcesses   int64
	MaxRunDirBytes int64
	MaxFileBytes   int64
}

func (l *Limits) validate() error {
	positive := []int64{
		int64(l.Timeout), l.MaxSourceBytes, l.MaxStdinBytes, l.MaxStdoutBytes, l.MaxStderrBytes,
		l.MaxMemoryBytes, l.MaxProcesses, l.MaxRunDirBytes, l.MaxFileBytes,
	}
	for _, value := range positive {
		if value <= 0 {
			return Errorf(ErrorCodeInvalidArgument, "every interpreter limit must be positive")
		}
	}
	if l.Timeout < time.Second {
		return Errorf(ErrorCodeInvalidArgument, "the interpreter timeout must be at least one second")
	}
	return nil
}

type Options struct {
	Store    *interpreter.Store
	Verified *interpreter.VerifiedSet
	RunRoot  string
	Limits   Limits
}

type sandboxRunner func(ctx context.Context, spec *sandbox.Spec, command string, args ...string) (sandbox.Result, error)

type Runner struct {
	store    *interpreter.Store
	verified *interpreter.VerifiedSet
	runRoot  string
	limits   Limits
	sandbox  sandboxRunner
	slots    chan struct{}
}

func NewRunner(options *Options) (*Runner, error) {
	if options == nil || options.Store == nil || options.Verified == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "store and verified set are required")
	}
	if !filepath.IsAbs(options.RunRoot) {
		return nil, Errorf(ErrorCodeInvalidArgument, "run root must be an absolute path")
	}
	if err := options.Limits.validate(); err != nil {
		return nil, err
	}
	return &Runner{
		store: options.Store, verified: options.Verified, runRoot: options.RunRoot, limits: options.Limits,
		sandbox: sandbox.Run, slots: make(chan struct{}, maxConcurrentRuns),
	}, nil
}

type Request struct {
	Runtime string
	Source  string
	Stdin   string
	Timeout time.Duration
}

type Outcome struct {
	Class           ResultClass
	ExitCode        int
	Signal          int
	Duration        time.Duration
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
}

type plan struct {
	bundle   interpreter.Bundle
	language language
	source   string
	stdin    string
	limits   sandbox.Limits
	cpu      time.Duration
}

func (r *Runner) Execute(ctx context.Context, request *Request) (Outcome, error) {
	if request == nil {
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if int64(len(request.Source)) > r.limits.MaxSourceBytes || int64(len(request.Stdin)) > r.limits.MaxStdinBytes {
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "source or stdin exceeds the configured limit")
	}
	language, supported := languageFor(request.Runtime)
	if !supported || !r.verified.Verified(request.Runtime) {
		return Outcome{}, Errorf(ErrorCodeUnavailable, "the %s runtime is not available", request.Runtime)
	}
	bundle, err := r.store.Bundle(request.Runtime)
	if err != nil {
		return Outcome{}, wrapf(ErrorCodeUnavailable, err, "the %s runtime is not available", request.Runtime)
	}
	timeout := r.limits.Timeout
	if request.Timeout > 0 && request.Timeout < timeout {
		timeout = max(request.Timeout, time.Second)
	}
	return r.run(ctx, &plan{
		bundle: bundle, language: language, source: request.Source, stdin: request.Stdin, cpu: timeout,
		limits: sandbox.Limits{
			MemoryBytes: r.limits.MaxMemoryBytes, CPUTime: timeout, MaxOutputBytes: max(r.limits.MaxStdoutBytes, r.limits.MaxStderrBytes),
			MaxOpenFiles: openFilesLimit, MaxProcesses: r.limits.MaxProcesses, FileBytes: r.limits.MaxFileBytes,
			Timeout: timeout + wallClockMargin, NoProcessRlimit: true, RunDirBytes: r.limits.MaxRunDirBytes,
		},
	})
}

func (r *Runner) run(ctx context.Context, p *plan) (Outcome, error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return Outcome{}, wrapf(ErrorCodeRunFailed, ctx.Err(), "run was cancelled before it started")
	}
	dir, err := r.newRunDir()
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, p.language.source), []byte(p.source), sourceFileMode); err != nil {
		return Outcome{}, wrapf(ErrorCodeRunFailed, err, "write the program source")
	}
	argv := p.bundle.Runtime.Command(p.bundle.Executable, p.language.source)
	spec := &sandbox.Spec{
		Profile:       sandbox.ProfileInterpreter,
		WorkingDir:    dir,
		ReadOnlyPaths: []string{p.bundle.Dir},
		AllowEnv:      environment(dir, &p.bundle.Runtime),
		Stdin:         []byte(p.stdin),
		Limits:        p.limits,
	}
	started := time.Now()
	result, err := r.sandbox(ctx, spec, argv[0], argv[1:]...)
	elapsed := time.Since(started)
	if err != nil {
		return Outcome{}, wrapSandbox(err)
	}
	return Outcome{
		Class: classify(&result, p.cpu), ExitCode: result.ExitCode, Signal: result.Signal, Duration: elapsed,
		Stdout: result.Stdout, Stderr: result.Stderr, StdoutTruncated: result.StdoutTruncated, StderrTruncated: result.StderrTruncated,
	}, nil
}

func (r *Runner) newRunDir() (string, error) {
	if err := os.MkdirAll(r.runRoot, runDirMode); err != nil {
		return "", wrapf(ErrorCodeRunFailed, pathFree(err), "create the run root")
	}
	dir, err := os.MkdirTemp(r.runRoot, runDirPrefix)
	if err != nil {
		return "", wrapf(ErrorCodeRunFailed, pathFree(err), "create the run directory")
	}
	return dir, nil
}

func environment(dir string, runtime *interpreter.Runtime) []string {
	runtimeEntries := runtime.EnvironmentEntries()
	entries := make([]string, 0, 3+len(runtimeEntries))
	entries = append(entries, "HOME="+dir, "TMPDIR="+dir, "LANG=C.UTF-8")
	return append(entries, runtimeEntries...)
}

func wrapSandbox(err error) error {
	if code, ok := sandbox.CodeOf(err); ok {
		return wrapf(ErrorCodeRunFailed, err, "the sandbox refused the run (%s)", code)
	}
	return wrapf(ErrorCodeRunFailed, err, "the run failed")
}
