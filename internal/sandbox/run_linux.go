package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func negotiate() (Primitives, error) {
	cgroupV2, cgroupDetail := cgroupV2Available()
	return Primitives{
		UserNamespace: usernsAvailable(),
		PidNamespace:  pidNamespaceAvailable(os.DirFS(procRoot)),
		Seccomp:       seccompAvailable(),
		CgroupV2:      cgroupV2,
		Landlock:      landlockAvailable(),
		ProcessGroups: true,
		CgroupDetail:  cgroupDetail,
	}, nil
}

func usernsAvailable() bool {
	data, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return false
	}
	if gate, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		if strings.TrimSpace(string(gate)) == "0" {
			return false
		}
	}
	return usernsSyscallPermitted()
}

const procRoot = "/proc"

func pidNamespaceAvailable(proc fs.FS) bool {
	if _, err := fs.Stat(proc, "self/ns/pid"); err != nil {
		return false
	}
	data, err := fs.ReadFile(proc, "sys/user/max_pid_namespaces")
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return err == nil && n > 0
}

func usernsSyscallPermitted() bool {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		result <- unix.Unshare(unix.CLONE_NEWUSER)
	}()
	err := <-result
	return !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.ENOSPC)
}

func cgroupV2Available() (available bool, detail string) {
	var mountInfo [64 << 10]byte
	fd, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, "cgroup v2 mount table unreadable"
	}
	defer func() { _ = fd.Close() }()
	n, _ := fd.Read(mountInfo[:])
	if !strings.Contains(string(mountInfo[:n]), "cgroup2 ") {
		return false, "cgroup v2 is not mounted"
	}
	root, err := hostCgroups().available()
	if err != nil {
		return false, errorDetail(err)
	}
	return true, "cgroup_root=" + displayName(root.name) + " mode=" + string(root.mode)
}

func errorDetail(err error) string {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Detail
	}
	return err.Error()
}

func landlockAvailable() bool {
	abi, err := landlockABI()
	return err == nil && abi >= 1
}

func resolveExecutable(command string) (string, error) {
	if strings.Contains(command, "/") {
		return command, nil
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", Errorf(ErrorCodeSandboxUnavailable, "resolve %q: %v", command, err)
	}
	return resolved, nil
}

func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:     true,
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
}

func run(ctx context.Context, spec *Spec, _ Primitives, command string, args ...string) (Result, error) {
	return runWithCgroups(ctx, hostCgroups(), spec, command, args...)
}

func runWithCgroups(ctx context.Context, cgroups *cgroupHost, spec *Spec, command string, args ...string) (Result, error) {
	if spec.AllowNetwork {
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "network access is not available in this sandbox")
	}
	resolved, err := resolveExecutable(command)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "start child: %v", err)
	}
	spec, scratch, err := withScratch(spec)
	if err != nil {
		return Result{}, err
	}
	defer removeScratch(scratch)
	payload, err := json.Marshal(childConfig{
		Profile: spec.Profile, WorkingDir: spec.WorkingDir, ReadOnlyPaths: spec.ReadOnlyPaths, ReadWritePaths: spec.ReadWritePaths,
		AllowEnv: spec.AllowEnv, Limits: spec.Limits, Command: resolved, Args: args,
	})
	if err != nil {
		return Result{}, Errorf(ErrorCodeSandboxInitFailed, "encode child config: %v", err)
	}
	configR, configW, err := os.Pipe()
	if err != nil {
		return Result{}, Errorf(ErrorCodeSandboxInitFailed, "config pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = configR.Close()
		_ = configW.Close()
		return Result{}, Errorf(ErrorCodeSandboxInitFailed, "error pipe: %v", err)
	}

	cg, err := cgroups.newCgroup(&spec.Limits)
	if err == nil && spec.Profile == ProfileInterpreter {
		if err = cg.killAsGroup(); err != nil {
			_ = cg.destroy()
		}
	}
	if err != nil {
		_ = configR.Close()
		_ = configW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return Result{}, err
	}

	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/proc/self/exe", ChildSentinel)
	cmd.Dir = spec.WorkingDir
	cmd.Env = append([]string(nil), spec.AllowEnv...)
	cmd.ExtraFiles = []*os.File{configR, errW}
	cmd.SysProcAttr = childSysProcAttr()
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	var stdout, stderr limitedBuffer
	if spec.Limits.MaxOutputBytes > 0 {
		stdout.limit = spec.Limits.MaxOutputBytes
		stderr.limit = spec.Limits.MaxOutputBytes
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = configR.Close()
		_ = configW.Close()
		_ = errW.Close()
		_ = errR.Close()
		_ = cg.destroy()
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "start child: %v", err)
	}
	_ = configR.Close() // the child owns its dup; the parent's copy is no longer needed
	_, _ = configW.Write(payload)
	_ = configW.Close()
	_ = errW.Close() // the child is the sole writer of the init-error pipe
	if err := cg.attach(cmd.Process.Pid); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = cg.destroy()
		return Result{}, Errorf(ErrorCodeSandboxInitFailed, "attach child to cgroup: %v", pathlessCause(err))
	}

	timeout := time.NewTimer(spec.Limits.Timeout)
	defer timeout.Stop()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	initErrCh := make(chan error, 1)
	go func() {
		initErr, _ := io.ReadAll(errR)
		_ = errR.Close()
		if len(initErr) > 0 {
			initErrCh <- Errorf(ErrorCodeSandboxInitFailed, "child setup: %s", strings.TrimSpace(string(initErr)))
		} else {
			initErrCh <- nil
		}
	}()
	var watcher *runDirWatcher
	var diskExceeded <-chan struct{}
	if spec.Limits.RunDirBytes > 0 {
		watcher = startRunDirWatcher(ctx, spec.WorkingDir, spec.Limits.RunDirBytes)
		diskExceeded = watcher.exceeded
	}

	var end runEnd
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-timeout.C:
		end = runEnd{terminated: true, timedOut: true}
	case <-ctx.Done():
		end = runEnd{terminated: true}
	case <-diskExceeded:
		end = runEnd{terminated: true, diskLimited: true}
	}
	if end.terminated {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		waitErr = <-waitDone
	}
	initErr := <-initErrCh
	events := cg.events()
	_ = cg.destroy()
	if watcher != nil {
		watcher.stop()
		usage := measureRunDir(context.WithoutCancel(ctx), spec.WorkingDir, defaultRunDirBounds())
		end.diskLimited = end.diskLimited || usage.exceeds(spec.Limits.RunDirBytes)
		end.fileLimited = spec.Limits.FileBytes > 0 && usage.largest >= spec.Limits.FileBytes
	}
	if initErr != nil {
		if end.terminated {
			return terminatedResult(&stdout, &stderr, cmd, events, end), initErr
		}
		return Result{}, initErr
	}
	if end.terminated {
		return terminatedResult(&stdout, &stderr, cmd, events, end), nil
	}
	if waitErr != nil && !isExitErr(waitErr) {
		return Result{}, waitErr
	}
	return exitedResult(&stdout, &stderr, cmd, events, end, waitErr), nil
}

type runEnd struct {
	terminated  bool
	timedOut    bool
	diskLimited bool
	fileLimited bool
}

func terminatedResult(stdout, stderr *limitedBuffer, cmd *exec.Cmd, events cgroupEvents, end runEnd) Result {
	result := Result{
		Stdout: stdout.String(), Stderr: stderr.String(), Output: stdout.String() + stderr.String(),
		Terminated: true, TimedOut: end.timedOut,
	}
	return observed(&result, stdout, stderr, cmd, events, end)
}

func exitedResult(stdout, stderr *limitedBuffer, cmd *exec.Cmd, events cgroupEvents, end runEnd, waitErr error) Result {
	out := strings.TrimRight(stdout.String(), "\n")
	errOut := strings.TrimRight(stderr.String(), "\n")
	result := Result{Output: strings.TrimRight(out+errOut, "\n"), Stdout: out, Stderr: errOut}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	}
	return observed(&result, stdout, stderr, cmd, events, end)
}

func observed(result *Result, stdout, stderr *limitedBuffer, cmd *exec.Cmd, events cgroupEvents, end runEnd) Result {
	result.StdoutTruncated = stdout.truncated
	result.StderrTruncated = stderr.truncated
	result.Truncated = stdout.truncated || stderr.truncated
	result.OOMKilled = events.oomKills > 0
	result.PidsLimited = events.pidsMax > 0
	result.DiskLimited = end.diskLimited
	result.FileLimited = end.fileLimited
	if state := cmd.ProcessState; state != nil {
		result.CPUTime = state.UserTime() + state.SystemTime()
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = int(status.Signal())
			result.ExitCode = -1
		}
	}
	return *result
}

func isExitErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

type limitedBuffer struct {
	limit     int64
	buffer    bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.buffer.Len()+len(p) > int(b.limit) {
		remaining := int(b.limit) - b.buffer.Len()
		if remaining > 0 {
			_, _ = b.buffer.Write(p[:remaining])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buffer.Write(p)
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}

var _ io.Writer = (*limitedBuffer)(nil)
