package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// negotiate probes the running kernel for the primitives the containment
// contract depends on. Process groups are always available on Linux, so
// negotiation never fails on this platform; the probe results let callers
// decide whether kernel-level denial (Landlock, seccomp, cgroup v2) is
// available before running untrusted children.
func negotiate() (Primitives, error) {
	return Primitives{
		UserNamespace: usernsAvailable(),
		Seccomp:       seccompAvailable(),
		CgroupV2:      cgroupV2Available(),
		Landlock:      landlockAvailable(),
		ProcessGroups: true,
	}, nil
}

// usernsAvailable reports whether an unprivileged process may create a user
// namespace. /proc/sys/user/max_user_namespaces is the kernel's authoritative
// knob: 0 (or absent) means user namespaces are disabled or compiled out; a
// positive value is the per-user creation limit. Some distros additionally
// gate unprivileged creation via kernel.unprivileged_userns_clone. Reading
// these knobs queries the kernel directly rather than inferring support from
// a marketing kernel version.
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
	return true
}

func cgroupV2Available() bool {
	var mountInfo [64 << 10]byte
	fd, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer func() { _ = fd.Close() }()
	n, _ := fd.Read(mountInfo[:])
	if !strings.Contains(string(mountInfo[:n]), "cgroup2 ") {
		return false
	}
	return cgroupControllersWritable()
}

func landlockAvailable() bool {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "landlock")
}

// run launches the tool inside a contained child. The child is a re-execution
// of this binary under the sandbox sentinel: the parent streams the config
// over a pipe, the child applies rlimits, Landlock, and seccomp then execs
// the target in fresh user and network namespaces. Output is capped at
// MaxOutputBytes. Timeout or cancellation kills the entire process group
// with SIGKILL and reaps it, so nothing survives the deadline. The cgroup is
// applied where the host delegated its controllers; Require gates the
// capability on that delegation, so production runs always enforce it.
func run(ctx context.Context, spec *Spec, _ Primitives, command string, args ...string) (Result, error) {
	if spec.AllowNetwork {
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "network access is not available in this sandbox")
	}
	resolved := command
	if !strings.Contains(command, "/") {
		lp, lerr := exec.LookPath(command)
		if lerr != nil {
			return Result{}, Errorf(ErrorCodeSandboxUnavailable, "resolve %q: %v", command, lerr)
		}
		resolved = lp
	}
	payload, err := json.Marshal(childConfig{
		WorkingDir: spec.WorkingDir, ReadOnlyPaths: spec.ReadOnlyPaths, ReadWritePaths: spec.ReadWritePaths,
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

	var cg *cgroup
	if cgroupControllersWritable() {
		c, cerr := newCgroup(spec.Limits)
		if cerr != nil {
			_ = configR.Close()
			_ = configW.Close()
			_ = errR.Close()
			_ = errW.Close()
			return Result{}, cerr
		}
		cg = &c
	}

	cmd := exec.CommandContext(ctx, "/proc/self/exe", ChildSentinel)
	cmd.Dir = spec.WorkingDir
	cmd.Env = append([]string(nil), spec.AllowEnv...)
	cmd.ExtraFiles = []*os.File{configR, errW}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:     true,
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	var output limitedBuffer
	if spec.Limits.MaxOutputBytes > 0 {
		output.limit = spec.Limits.MaxOutputBytes
	}
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		_ = configR.Close()
		_ = configW.Close()
		_ = errW.Close()
		_ = errR.Close()
		if cg != nil {
			_ = cg.destroy()
		}
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "start child: %v", err)
	}
	_ = configR.Close() // the child owns its dup; the parent's copy is no longer needed
	_, _ = configW.Write(payload)
	_ = configW.Close()
	_ = errW.Close() // the child is the sole writer of the init-error pipe
	if cg != nil {
		if err := cg.attach(cmd.Process.Pid); err != nil {
			// A child that escapes its cgroup runs without memory/PID
			// enforcement; kill it and refuse rather than fail open.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			_ = cg.destroy()
			return Result{}, Errorf(ErrorCodeSandboxInitFailed, "attach child to cgroup: %v", err)
		}
	}

	timeout := time.NewTimer(spec.Limits.Timeout)
	defer timeout.Stop()
	// waitDone and initErrCh are buffered so both senders always terminate;
	// the err reader is authoritative for setup failure, the wait reaps.
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

	buildResult := func(waitErr error) Result {
		result := Result{Output: strings.TrimRight(output.String(), "\n"), Truncated: output.truncated}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		return result
	}

	// The process-group kill is a second net so grandchildren die even though
	// only the parent is held directly.
	select {
	case waitErr := <-waitDone:
		initErr := <-initErrCh // child exit closed the err pipe, so the reader has finished
		if cg != nil {
			_ = cg.destroy()
		}
		if initErr != nil {
			return Result{}, initErr
		}
		if waitErr != nil && !isExitErr(waitErr) {
			return Result{}, waitErr
		}
		return buildResult(waitErr), nil
	case <-timeout.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone
		initErr := <-initErrCh
		if cg != nil {
			_ = cg.destroy()
		}
		if initErr != nil {
			return Result{Terminated: true, Truncated: output.truncated}, initErr
		}
		return Result{Terminated: true, Truncated: output.truncated}, nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone
		initErr := <-initErrCh
		if cg != nil {
			_ = cg.destroy()
		}
		if initErr != nil {
			return Result{Terminated: true, Truncated: output.truncated}, initErr
		}
		return Result{Terminated: true, Truncated: output.truncated}, nil
	}
}

func isExitErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// limitedBuffer accumulates child output up to a byte cap and drops the
// rest, so a spamming child cannot exhaust parent memory.
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
