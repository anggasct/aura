//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// sessionCloseGrace is how long a closing session waits between SIGTERM and
// SIGKILL for the child group to exit on its own.
const sessionCloseGrace = 2 * time.Second

// sessionSetupWait bounds how long Start waits for the child to report its
// isolation setup result. Child setup is sub-second in practice; the bound
// only matters for a wedged host.
const sessionSetupWait = 10 * time.Second

// sessionDefaultOutputLimit bounds one exchange when the request sets no
// output limit, so a chatty child can never grow an exchange without bound.
const sessionDefaultOutputLimit = 1 << 20

// sessionIODeadline covers the phases whose bounds come from the exchange
// deadline; a caller without any deadline still gets a finite wait.
const sessionIODeadline = 5 * time.Second

// startSession launches the long-lived confined child. The confinement setup
// mirrors the one-shot run path — the same re-executed child, config pipe,
// init-error pipe, cgroup attach, and fail-closed error codes — except the
// child's stdin and stdout stay connected to the parent for repeated
// exchanges and no whole-run deadline is armed here. The child's isolation
// setup result is awaited before Start returns, so a failed setup leaves no
// process behind.
//
// A lifecycle watcher is armed before the first exchange: cancellation of
// the Start context tears the group down exactly like Close (TERM→KILL→reap,
// cgroup release). The child command observes the context's payload only,
// never its cancellation — teardown is the watcher's and Close's job, so
// both paths share one termination sequence.
func startSession(ctx context.Context, req *SessionRequest, spec *Spec) (*Session, error) {
	resolved, err := resolveExecutable(req.Executable)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(childConfig{
		WorkingDir: spec.WorkingDir, ReadOnlyPaths: spec.ReadOnlyPaths, ReadWritePaths: spec.ReadWritePaths,
		AllowEnv: spec.AllowEnv, Limits: spec.Limits, Command: resolved, Args: req.Arguments,
	})
	if err != nil {
		return nil, Errorf(ErrorCodeSandboxInitFailed, "encode child config: %v", err)
	}
	configR, configW, err := os.Pipe()
	if err != nil {
		return nil, Errorf(ErrorCodeSandboxInitFailed, "config pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = configR.Close()
		_ = configW.Close()
		return nil, Errorf(ErrorCodeSandboxInitFailed, "error pipe: %v", err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		_ = configR.Close()
		_ = configW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, Errorf(ErrorCodeSandboxInitFailed, "stdin pipe: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = configR.Close()
		_ = configW.Close()
		_ = errR.Close()
		_ = errW.Close()
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, Errorf(ErrorCodeSandboxInitFailed, "stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = configR.Close()
		_ = configW.Close()
		_ = errR.Close()
		_ = errW.Close()
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, Errorf(ErrorCodeSandboxInitFailed, "stderr pipe: %v", err)
	}

	var cg *cgroup
	if cgroupControllersWritable() {
		c, cerr := newCgroup(spec.Limits)
		if cerr != nil {
			closeAll(configR, configW, errR, errW, stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
			return nil, cerr
		}
		cg = &c
	}

	// The child command observes the context's payload but not its
	// cancellation: the session's lifetime is owned by Close and by the
	// lifecycle watcher armed below, and both run the same explicit
	// termination sequence. If Start's caller already cancelled (or cancels
	// during setup), the abort path reaps; otherwise the watcher takes over
	// once the session is fully constructed.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/proc/self/exe", ChildSentinel)
	cmd.Dir = spec.WorkingDir
	cmd.Env = append([]string(nil), spec.AllowEnv...)
	// The persistent stdio pipes ride the command's own stdin/stdout/stderr
	// slots, so the re-execution path dups them onto fds 0, 1, and 2 exactly
	// as it would for any child; fds 3 and 4 remain the config and
	// init-error pipes the one-shot child expects.
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.ExtraFiles = []*os.File{configR, errW}
	cmd.SysProcAttr = childSysProcAttr()

	ls := &linuxSession{
		req:       *req,
		pid:       -1,
		cmd:       cmd,
		cg:        cg,
		stdin:     stdinW,
		stdout:    stdoutR,
		stderr:    stderrR,
		reader:    bufio.NewReader(stdoutR),
		reapDone:  make(chan struct{}),
		brokenTap: make(chan struct{}),
	}
	abort := func(setupErr error) (*Session, error) {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-ls.reapDone
		}
		closeAll(configR, configW, errR, errW, stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		if cg != nil {
			_ = cg.destroy()
		}
		return nil, setupErr
	}
	if err := cmd.Start(); err != nil {
		return abort(Errorf(ErrorCodeSandboxUnavailable, "start child: %v", err))
	}
	ls.pid = cmd.Process.Pid
	go func() {
		ls.reapErr = cmd.Wait()
		close(ls.reapDone)
	}()
	// The child owns its dups of these; the parent copies are spent.
	_ = configR.Close()
	_, _ = configW.Write(payload)
	_ = configW.Close()
	_ = errW.Close()
	_ = stdinR.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	if cg != nil {
		if err := cg.attach(ls.pid); err != nil {
			// A child that escapes its cgroup runs without memory/PID
			// enforcement; kill the group and refuse rather than fail open.
			return abort(Errorf(ErrorCodeSandboxInitFailed, "attach child to cgroup: %v", err))
		}
	}

	if err := awaitChildSetup(errR, min(req.Limits.Timeout, sessionSetupWait)); err != nil {
		return abort(err)
	}
	closeAll(errR)
	// The session is fully live: hand lifecycle ownership to the watcher so
	// a cancelled Start context tears the group down exactly like Close.
	ls.armWatch(ctx)
	return &Session{sessionAPI: ls}, nil
}

// awaitChildSetup drains the init-error pipe until the child reports a setup
// failure or closes the pipe after a clean setup — the same signal the
// one-shot run path waits on. A wedged child (no result within the bound)
// fails closed like any other setup failure.
func awaitChildSetup(errR *os.File, limit time.Duration) error {
	_ = errR.SetReadDeadline(time.Now().Add(limit))
	initErr, readErr := io.ReadAll(errR)
	if len(initErr) > 0 {
		return Errorf(ErrorCodeSandboxInitFailed, "child setup: %s", strings.TrimSpace(string(initErr)))
	}
	if readErr != nil {
		return Errorf(ErrorCodeSandboxInitFailed, "child setup did not complete: %v", readErr)
	}
	return nil
}

type linuxSession struct {
	// mu serializes Request calls over the child's single stdio stream: one
	// protocol exchange at a time, from write to drained response.
	mu     sync.Mutex
	req    SessionRequest
	pid    int
	cmd    *exec.Cmd
	cg     *cgroup
	stdin  *os.File
	stderr *os.File
	stdout *os.File
	reader *bufio.Reader
	// reapDone closes when the direct child has been reaped; reapErr is its
	// wait result, readable once reapDone is closed.
	reapDone chan struct{}
	reapErr  error
	closed   bool
	// watchStop cancels the lifecycle watcher's derived context. Written
	// once by armWatch before the session is published, read by close under
	// mu, so the two lifecycle owners cannot interleave: either armWatch
	// arms first and close cancels the watcher, or close wins the flag and
	// no watcher is armed at all.
	watchStop context.CancelFunc
	// pendingLine records an exchange abandoned mid-response (deadline miss).
	// The child's response is still in flight; the next exchange drains to
	// the next newline — deterministically the rest of that response, since
	// responses strictly alternate with requests — before writing its own.
	pendingLine bool
	// broken seals the session after child exit or stream desync; every
	// later Request fails closed. brokenTap is closed once, at that point.
	brokenFlag atomic.Bool
	brokenTap  chan struct{}
	brokeOnce  sync.Once
}

var _ sessionAPI = (*linuxSession)(nil)

func (ls *linuxSession) markBroken() {
	ls.brokeOnce.Do(func() {
		ls.brokenFlag.Store(true)
		close(ls.brokenTap)
	})
}

func (ls *linuxSession) isBroken() bool {
	return ls.brokenFlag.Load()
}

// armWatch starts the lifecycle watcher: when the Start context is
// cancelled, the session tears down exactly as if Close had been called —
// group TERM→KILL, reap, cgroup release. The watcher watches a context
// derived from the Start context so it can always be released: Close cancels
// the derived context before killing, so no goroutine ever lingers blocked
// on a Start context that is never cancelled, and exactly one owner ever
// drives the termination sequence.
func (ls *linuxSession) armWatch(ctx context.Context) {
	if ctx == nil {
		return
	}
	watchC, watchStop := context.WithCancel(ctx)
	ls.mu.Lock()
	if ls.closed {
		// Close won the race before the session was even published; nothing
		// to watch.
		ls.mu.Unlock()
		watchStop()
		return
	}
	ls.watchStop = watchStop
	ls.mu.Unlock()
	go func() {
		<-watchC.Done()
		watchStop() // release the child context's plumbing either way
		_ = ls.close(watchC)
	}()
}

// close terminates the session child: SIGTERM to the whole process group,
// SIGKILL after the grace period, reap, cgroup release. Idempotent — the
// first caller (Close or the Start-context watcher) runs the sequence once,
// and every later call returns nil.
func (ls *linuxSession) close(_ context.Context) error {
	ls.mu.Lock()
	if ls.closed || ls.pid <= 0 {
		ls.mu.Unlock()
		return nil
	}
	ls.closed = true
	pid := ls.pid
	// Detach the watcher before killing: after closed is set the watcher
	// would be a no-op anyway, but stopping it means no goroutine lingers
	// blocked on the (possibly never-cancelled) Start context after an
	// explicit Close.
	watchStop := ls.watchStop
	ls.watchStop = nil
	ls.mu.Unlock()
	if watchStop != nil {
		watchStop()
	}

	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-ls.reapDone:
	case <-time.After(sessionCloseGrace):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-ls.reapDone
	}
	closeAll(ls.stdin, ls.stdout, ls.stderr)
	ls.markBroken()
	if ls.cg != nil {
		_ = ls.cg.destroy()
	}
	return nil
}

func (ls *linuxSession) request(ctx context.Context, payload []byte) ([]byte, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return nil, Errorf(ErrorCodeSessionClosed, "session is closed")
	}
	if ls.isBroken() {
		return nil, Errorf(ErrorCodeSessionBroken, "session child is gone")
	}

	exchange, cancel := context.WithTimeout(ctx, ls.exchangeTimeout(ctx))
	defer cancel()

	// Recover a previously abandoned exchange first: discard the in-flight
	// response up to its newline so this exchange starts aligned. The
	// remaining exchange budget bounds the drain.
	if ls.pendingLine {
		if err := ls.stdout.SetReadDeadline(ioDeadline(exchange)); err != nil {
			ls.markBroken()
			return nil, Errorf(ErrorCodeSessionBroken, "arm recovery drain: %v", err)
		}
		err := ls.drainLine()
		_ = ls.stdout.SetReadDeadline(time.Time{})
		if err != nil {
			if isDeadline(err) {
				return nil, Errorf(ErrorCodeSandboxTimeout, "recovery drain past deadline: %v", err)
			}
			ls.markBroken()
			return nil, Errorf(ErrorCodeSessionBroken, "recovery drain: %v", err)
		}
		ls.pendingLine = false
	}

	sent, err := writeFrame(exchange, ls.stdin, payload)
	if err != nil {
		if sent > 0 || !isDeadline(err) {
			// A partial frame is already in the child's stdin, or the pipe
			// failed outright; the stream can no longer be aligned to an
			// exchange boundary.
			ls.markBroken()
			return nil, Errorf(ErrorCodeSessionBroken, "write to session child: %v", err)
		}
		return nil, Errorf(ErrorCodeSandboxTimeout, "request not sent before deadline: %v", err)
	}

	response, err := ls.readLine(exchange)
	switch {
	case err == nil:
		return response, nil
	case isDeadline(err):
		// The response is still in flight; the next exchange drains it.
		ls.pendingLine = true
		return nil, Errorf(ErrorCodeSandboxTimeout, "response past deadline: %v", err)
	case isOutputExceeded(err):
		// Drain the oversized line to its newline so the session stays
		// usable; a drain that itself times out defers recovery.
		if derr := ls.stdout.SetReadDeadline(ioDeadline(exchange)); derr == nil {
			derr = ls.drainLine()
			_ = ls.stdout.SetReadDeadline(time.Time{})
			if derr != nil {
				if isDeadline(derr) {
					ls.pendingLine = true
					return nil, err
				}
				ls.markBroken()
				return nil, Errorf(ErrorCodeSessionBroken, "drain oversized response: %v", derr)
			}
		} else {
			ls.markBroken()
			return nil, Errorf(ErrorCodeSessionBroken, "arm response drain: %v", derr)
		}
		return nil, err
	default:
		ls.markBroken()
		return nil, Errorf(ErrorCodeSessionBroken, "read from session child: %v", err)
	}
}

// readLine reads one newline-terminated response line, bounded by the
// configured output limit (or the default bound when unset).
func (ls *linuxSession) readLine(exchange context.Context) ([]byte, error) {
	if err := ls.stdout.SetReadDeadline(ioDeadline(exchange)); err != nil {
		return nil, err
	}
	defer func() { _ = ls.stdout.SetReadDeadline(time.Time{}) }()
	limit := ls.outputLimit()
	var out []byte
	for {
		frag, err := ls.reader.ReadSlice('\n')
		out = append(out, frag...)
		if int64(len(out)) > limit {
			return nil, Errorf(ErrorCodeSandboxOutputExceeded, "response exceeds %d bytes", limit)
		}
		if err == nil {
			return bytes.TrimSuffix(out, []byte("\n")), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			msg := "child closed stdout mid-exchange"
			if cause := ls.exitCause(); cause != "" {
				msg += "; " + cause
			}
			return nil, Errorf(ErrorCodeSessionBroken, "%s", msg)
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, Errorf(ErrorCodeSandboxTimeout, "response read past deadline")
		}
		return nil, err
	}
}

// drainLine consumes up to and including the next newline, discarding bytes.
// From the current stream position that is deterministically the rest of the
// abandoned response, because responses strictly alternate with requests.
// The caller arms the read deadline.
func (ls *linuxSession) drainLine() error {
	for {
		_, err := ls.reader.ReadSlice('\n')
		if err == nil {
			return nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return Errorf(ErrorCodeSessionBroken, "child closed stdout during drain")
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return Errorf(ErrorCodeSandboxTimeout, "drain past deadline")
		}
		return err
	}
}

func (ls *linuxSession) outputLimit() int64 {
	if ls.req.Limits.MaxOutputBytes > 0 {
		return ls.req.Limits.MaxOutputBytes
	}
	return sessionDefaultOutputLimit
}

// exitCause describes how the child process ended, for error messages. It
// never blocks: a child that has not been reaped reports nothing.
func (ls *linuxSession) exitCause() string {
	select {
	case <-ls.reapDone:
	default:
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(ls.reapErr, &exitErr) {
		return fmt.Sprintf("child exit: %v", exitErr)
	}
	if ls.reapErr != nil {
		return fmt.Sprintf("child wait: %v", ls.reapErr)
	}
	return "child exited cleanly"
}

// exchangeTimeout is the per-exchange deadline: the sooner of the caller's
// remaining context deadline (if any) and the configured timeout.
func (ls *linuxSession) exchangeTimeout(ctx context.Context) time.Duration {
	timeout := ls.req.Limits.Timeout
	deadline, ok := ctx.Deadline()
	if !ok {
		return timeout
	}
	return min(time.Until(deadline), timeout)
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// writeFrame writes payload plus the newline delimiter under the exchange
// deadline. It reports how many bytes were written so a partial frame can be
// distinguished from a clean miss.
func writeFrame(exchange context.Context, stdin *os.File, payload []byte) (int, error) {
	if err := stdin.SetWriteDeadline(ioDeadline(exchange)); err != nil {
		return 0, err
	}
	defer func() { _ = stdin.SetWriteDeadline(time.Time{}) }()
	frame := make([]byte, 0, len(payload)+1)
	frame = append(frame, payload...)
	frame = append(frame, '\n')
	return stdin.Write(frame)
}

// ioDeadline is the absolute deadline for one I/O phase: the exchange's own
// deadline when present, else a finite default so a deadline-less caller
// still cannot block forever.
func ioDeadline(exchange context.Context) time.Time {
	if deadline, ok := exchange.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(sessionIODeadline)
}

func isDeadline(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	code, _ := CodeOf(err)
	return code == ErrorCodeSandboxTimeout
}

func isOutputExceeded(err error) bool {
	code, _ := CodeOf(err)
	return code == ErrorCodeSandboxOutputExceeded
}
