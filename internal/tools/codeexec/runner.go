package codeexec

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools"
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

	buildMemoryBytes = 1 << 30
	buildProcesses   = 128
	buildFileBytes   = 256 << 20
	buildRunDirBytes = 512 << 20
)

type Limits struct {
	Timeout        time.Duration
	BuildTimeout   time.Duration
	MaxSourceBytes int64
	MaxStdinBytes  int64
	MaxStdoutBytes int64
	MaxStderrBytes int64
	MaxMemoryBytes int64
	MaxProcesses   int64
	MaxRunDirBytes int64
	MaxFileBytes   int64
	MaxOutputFiles int64
}

func (l *Limits) validate() error {
	positive := []int64{
		int64(l.Timeout), int64(l.BuildTimeout), l.MaxSourceBytes, l.MaxStdinBytes, l.MaxStdoutBytes, l.MaxStderrBytes,
		l.MaxMemoryBytes, l.MaxProcesses, l.MaxRunDirBytes, l.MaxFileBytes, l.MaxOutputFiles,
	}
	for _, value := range positive {
		if value <= 0 {
			return Errorf(ErrorCodeInvalidArgument, "every interpreter limit must be positive")
		}
	}
	if l.MaxOutputFiles > tools.MaxCodeOutputFiles {
		return Errorf(ErrorCodeInvalidArgument, "the output file limit must be at most %d", tools.MaxCodeOutputFiles)
	}
	if l.Timeout < time.Second || l.BuildTimeout < time.Second {
		return Errorf(ErrorCodeInvalidArgument, "the interpreter timeouts must be at least one second")
	}
	return nil
}

type Options struct {
	Store     *interpreter.Store
	Verified  *interpreter.VerifiedSet
	Artifacts ArtifactSink
	Observer  Observer
	RunRoot   string
	Limits    Limits
}

type sandboxRunner func(ctx context.Context, spec *sandbox.Spec, command string, args ...string) (sandbox.Result, error)

type Runner struct {
	store     *interpreter.Store
	verified  *interpreter.VerifiedSet
	artifacts ArtifactSink
	observer  Observer
	runRoot   string
	limits    Limits
	sandbox   sandboxRunner
	slots     chan struct{}
}

func NewRunner(options *Options) (*Runner, error) {
	if options == nil || options.Store == nil || options.Verified == nil || options.Artifacts == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "store, verified set and artifact sink are required")
	}
	if !filepath.IsAbs(options.RunRoot) {
		return nil, Errorf(ErrorCodeInvalidArgument, "run root must be an absolute path")
	}
	if err := options.Limits.validate(); err != nil {
		return nil, err
	}
	return &Runner{
		store: options.Store, verified: options.Verified, artifacts: options.Artifacts, observer: options.Observer, runRoot: options.RunRoot, limits: options.Limits,
		sandbox: sandbox.Run, slots: make(chan struct{}, maxConcurrentRuns),
	}, nil
}

type Request struct {
	Runtime     string
	Source      string
	Stdin       string
	SessionID   string
	RequestID   string
	OutputFiles []string
	Timeout     time.Duration
}

type Outcome struct {
	Class           ResultClass
	Runtime         string
	RuntimeVersion  string
	Phase           Phase
	ExitCode        int
	Signal          int
	Duration        time.Duration
	Build           Usage
	Run             Usage
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	Artifacts       []ArtifactEntry
}

type plan struct {
	bundle   interpreter.Bundle
	language language
	source   string
	stdin    string
	outputs  []string
	origin   provenance
	limits   sandbox.Limits
	cpu      time.Duration
	build    time.Duration
}

func (r *Runner) Execute(ctx context.Context, request *Request) (Outcome, error) {
	if request == nil {
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if int64(len(request.Source)) > r.limits.MaxSourceBytes || int64(len(request.Stdin)) > r.limits.MaxStdinBytes {
		return Outcome{}, Errorf(ErrorCodeInvalidArgument, "source or stdin exceeds the configured limit")
	}
	if err := tools.ValidateCodeOutputFiles(request.OutputFiles, int(r.limits.MaxOutputFiles)); err != nil {
		return Outcome{}, wrapf(ErrorCodeInvalidArgument, err, "%v", err)
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
	origin, err := newProvenance(request.SessionID, request.RequestID, bundle.Runtime.ID, bundle.Runtime.Version)
	if err != nil {
		return Outcome{}, err
	}
	outcome, err := r.run(ctx, &plan{
		bundle: bundle, language: language, source: request.Source, stdin: request.Stdin, cpu: timeout,
		outputs: request.OutputFiles, origin: origin, build: r.limits.BuildTimeout,
		limits: sandbox.Limits{
			MemoryBytes: r.limits.MaxMemoryBytes, CPUTime: timeout, MaxOutputBytes: max(r.limits.MaxStdoutBytes, r.limits.MaxStderrBytes),
			MaxOpenFiles: openFilesLimit, MaxProcesses: r.limits.MaxProcesses, FileBytes: r.limits.MaxFileBytes,
			Timeout: timeout + wallClockMargin, NoProcessRlimit: true, RunDirBytes: r.limits.MaxRunDirBytes,
		},
	})
	if err == nil && r.observer != nil {
		r.observer(ctx, newObservation(&outcome, bundle.Runtime.Builds()))
	}
	return outcome, err
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
	defer dir.release()
	if err := writeWorkspace(dir.path, p); err != nil {
		return Outcome{}, err
	}
	var buildUsage Usage
	program := ""
	readOnly := []string{p.bundle.Dir}
	if p.bundle.Runtime.Builds() {
		program = filepath.Join(dir.path, filepath.FromSlash(p.bundle.Runtime.Build.Program))
		stopped, built, err := r.build(ctx, p, dir, program)
		if err != nil {
			return Outcome{}, err
		}
		stopped.Runtime, stopped.RuntimeVersion = p.bundle.Runtime.ID, p.bundle.Runtime.Version
		stopped.Phase, stopped.Duration = PhaseBuild, stopped.Build.Duration
		if !built {
			return stopped, nil
		}
		buildUsage = stopped.Build
		readOnly = nil
	}
	argv := p.bundle.Runtime.Command(p.bundle.Executable, p.language.source, program)
	spec := &sandbox.Spec{
		Profile:       sandbox.ProfileInterpreter,
		WorkingDir:    dir.path,
		ReadOnlyPaths: readOnly,
		AllowEnv:      environment(dir.path, &p.bundle.Runtime),
		Stdin:         []byte(p.stdin),
		Limits:        p.limits,
	}
	runStarted := time.Now()
	result, err := r.sandbox(ctx, spec, argv[0], argv[1:]...)
	runUsage := Usage{Duration: time.Since(runStarted), CPUTime: result.CPUTime}
	if err != nil {
		return Outcome{}, wrapSandbox(err)
	}
	outcome := Outcome{
		Class: classify(&result, p.cpu), Runtime: p.bundle.Runtime.ID, RuntimeVersion: p.bundle.Runtime.Version, Phase: PhaseRun,
		ExitCode: result.ExitCode, Signal: result.Signal, Build: buildUsage, Run: runUsage, Duration: buildUsage.Duration + runUsage.Duration,
		Stdout: result.Stdout, Stderr: result.Stderr, StdoutTruncated: result.StdoutTruncated, StderrTruncated: result.StderrTruncated,
	}
	if outcome.Class == ClassOK || outcome.Class == ClassExitNonzero {
		imports := importer{sink: r.artifacts, maxFiles: int(r.limits.MaxOutputFiles), maxBytes: r.limits.MaxFileBytes}
		outcome.Artifacts = imports.importAll(ctx, dir.outputDir(), p.outputs, &p.origin)
	}
	return outcome, nil
}

func writeWorkspace(dir string, p *plan) error {
	for _, sub := range p.language.directories {
		if err := os.MkdirAll(filepath.Join(dir, sub), runDirMode); err != nil {
			return wrapf(ErrorCodeRunFailed, pathFree(err), "create the program workspace")
		}
	}
	files := []workspaceFile{{name: p.language.source, content: p.source}}
	if p.language.workspace != nil {
		files = append(files, p.language.workspace(&p.bundle)...)
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.name), []byte(file.content), sourceFileMode); err != nil {
			return wrapf(ErrorCodeRunFailed, pathFree(err), "write the program source")
		}
	}
	return nil
}

func (r *Runner) build(ctx context.Context, p *plan, dir *runDir, program string) (Outcome, bool, error) {
	argv := p.bundle.Runtime.BuildCommand(p.bundle.Executable, p.language.source, program)
	spec := &sandbox.Spec{
		Profile:       sandbox.ProfileInterpreter,
		WorkingDir:    dir.path,
		ReadOnlyPaths: []string{p.bundle.Dir},
		AllowEnv:      buildEnvironment(dir.path, &p.bundle, &p.language),
		Limits: sandbox.Limits{
			MemoryBytes: buildMemoryBytes, CPUTime: p.build, MaxOutputBytes: p.limits.MaxOutputBytes, MaxOpenFiles: openFilesLimit,
			MaxProcesses: buildProcesses, FileBytes: buildFileBytes, Timeout: p.build, NoProcessRlimit: true, RunDirBytes: buildRunDirBytes,
		},
	}
	started := time.Now()
	result, err := r.sandbox(ctx, spec, argv[0], argv[1:]...)
	usage := Usage{Duration: time.Since(started), CPUTime: result.CPUTime}
	if err != nil {
		return Outcome{}, false, wrapSandbox(err)
	}
	if class := classify(&result, p.build); class != ClassOK {
		return Outcome{
			Class: class, ExitCode: result.ExitCode, Signal: result.Signal, Stdout: result.Stdout, Stderr: result.Stderr,
			StdoutTruncated: result.StdoutTruncated, StderrTruncated: result.StderrTruncated, Build: usage,
		}, false, nil
	}
	for _, sub := range p.language.scratch {
		if err := dir.parent.RemoveAll(dir.name + "/" + sub); err != nil {
			return Outcome{}, false, wrapf(ErrorCodeRunFailed, pathFree(err), "remove the build scratch")
		}
	}
	select {
	case <-ctx.Done():
		return Outcome{Class: ClassCancelled, ExitCode: -1, Signal: int(syscall.SIGKILL), Build: usage}, false, nil
	default:
		return Outcome{Build: usage}, true, nil
	}
}

func baseEnvironment(dir string) []string {
	return []string{"HOME=" + dir, "TMPDIR=" + dir, "LANG=C.UTF-8"}
}

func environment(dir string, runtime *interpreter.Runtime) []string {
	return append(baseEnvironment(dir), runtime.EnvironmentEntries()...)
}

func buildEnvironment(dir string, bundle *interpreter.Bundle, lang *language) []string {
	entries := append(baseEnvironment(dir), bundle.Runtime.BuildEnvironmentEntries()...)
	if lang.buildEnvironment == nil {
		return entries
	}
	overrides := lang.buildEnvironment(dir, bundle)
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		entry := key + "=" + overrides[key]
		at := slices.IndexFunc(entries, func(existing string) bool { return strings.HasPrefix(existing, key+"=") })
		if at < 0 {
			entries = append(entries, entry)
			continue
		}
		entries[at] = entry
	}
	return entries
}

func wrapSandbox(err error) error {
	if code, ok := sandbox.CodeOf(err); ok {
		return wrapf(ErrorCodeRunFailed, err, "the sandbox refused the run (%s)", code)
	}
	return wrapf(ErrorCodeRunFailed, err, "the run failed")
}
