package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// negotiate probes the running kernel for the primitives the containment
// contract depends on. Process groups are always available on Linux, so
// negotiation never fails on this platform; the probe results let callers
// decide whether kernel-level denial (Landlock, seccomp, cgroup v2) is
// available before running untrusted children.
func negotiate() (Primitives, error) {
	return Primitives{
		Seccomp:       seccompAvailable(),
		CgroupV2:      cgroupV2Available(),
		Landlock:      landlockAvailable(),
		ProcessGroups: true,
	}, nil
}

// seccompAvailable probes kernel seccomp support directly instead of
// reading the current process's confinement mode from /proc/self/status,
// which is 0 on ordinary unconfined hosts even when CONFIG_SECCOMP=y.
// SECCOMP_GET_ACTION_AVAIL returns 0 when the action can be used, and
// ENOSYS when the seccomp syscall is not compiled in; EINVAL/EACCES/EPERM
// mean the syscall exists (kernel support present).
func seccompAvailable() bool {
	const (
		seccompGetActionAvail = 2
		seccompRetAllow       = 0x7fff0000
		sysSeccomp            = 317
	)
	action := seccompRetAllow
	_, _, errno := syscall.Syscall(sysSeccomp, seccompGetActionAvail, 0, uintptr(unsafe.Pointer(&action)))
	return errno != syscall.ENOSYS
}

func cgroupV2Available() bool {
	var mountInfo [64 << 10]byte
	fd, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer func() { _ = fd.Close() }()
	n, _ := fd.Read(mountInfo[:])
	return strings.Contains(string(mountInfo[:n]), "cgroup2 ")
}

func landlockAvailable() bool {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "landlock")
}

// run launches the child in its own process group with an allowlisted
// environment and the explicit working directory. Output is capped at
// MaxOutputBytes. Timeout or cancellation kills the entire process group
// with SIGKILL and reaps it, so nothing survives the deadline. On
// non-Linux the build-tag sibling fails closed before any process starts.
func run(ctx context.Context, spec *Spec, _ Primitives, command string, args ...string) (Result, error) {
	if spec.AllowNetwork {
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "network access is not available in this sandbox")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = spec.WorkingDir
	cmd.Env = append([]string(nil), spec.AllowEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	var output limitedBuffer
	if spec.Limits.MaxOutputBytes > 0 {
		output.limit = spec.Limits.MaxOutputBytes
	}
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "cannot start %q: %v", command, err)
	}

	// exec.Cmd.Wait handles SIGKILL-on-cancel; the process group kill is a
	// second net so grandchildren die even though we only hold the parent.
	timeout := time.NewTimer(spec.Limits.Timeout)
	defer timeout.Stop()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		result := Result{Output: strings.TrimRight(output.String(), "\n"), Truncated: output.truncated}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		return result, nil
	case <-timeout.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return Result{Terminated: true, Truncated: output.truncated}, nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return Result{Terminated: true, Truncated: output.truncated}, nil
	}
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
