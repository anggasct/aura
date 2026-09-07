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

const sessionCloseGrace = 2 * time.Second

const sessionSetupWait = 10 * time.Second

const sessionDefaultOutputLimit = 1 << 20

const sessionIODeadline = 5 * time.Second

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

	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/proc/self/exe", ChildSentinel)
	cmd.Dir = spec.WorkingDir
	cmd.Env = append([]string(nil), spec.AllowEnv...)
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
	ls.armWatch(ctx)
	return &Session{sessionAPI: ls}, nil
}

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
	mu          sync.Mutex
	req         SessionRequest
	pid         int
	cmd         *exec.Cmd
	cg          *cgroup
	stdin       *os.File
	stderr      *os.File
	stdout      *os.File
	reader      *bufio.Reader
	reapDone    chan struct{}
	reapErr     error
	closed      bool
	watchStop   context.CancelFunc
	pendingLine bool
	brokenFlag  atomic.Bool
	brokenTap   chan struct{}
	brokeOnce   sync.Once
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

func (ls *linuxSession) armWatch(ctx context.Context) {
	if ctx == nil {
		return
	}
	watchC, watchStop := context.WithCancel(ctx)
	ls.mu.Lock()
	if ls.closed {
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

func (ls *linuxSession) close(_ context.Context) error {
	ls.mu.Lock()
	if ls.closed || ls.pid <= 0 {
		ls.mu.Unlock()
		return nil
	}
	ls.closed = true
	pid := ls.pid
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
		ls.pendingLine = true
		return nil, Errorf(ErrorCodeSandboxTimeout, "response past deadline: %v", err)
	case isOutputExceeded(err):
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
