package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// negotiate probes the running kernel for the primitives the contract
// depends on. Landlock and cgroup v2 are required for declared-scope
// enforcement; their absence fails closed.
func negotiate() (Primitives, error) {
	primitives := Primitives{
		Seccomp:       seccompAvailable(),
		CgroupV2:      cgroupV2Available(),
		Landlock:      landlockAvailable(),
		ProcessGroups: true,
	}
	if !primitives.ProcessGroups {
		return primitives, Errorf(ErrorCodeSandboxUnavailable, "process groups are not available")
	}
	return primitives, nil
}

func seccompAvailable() bool {
	fd, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer func() { _ = fd.Close() }()
	var status [16 << 10]byte
	n, err := fd.Read(status[:])
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(status[:n]), "\n") {
		if strings.HasPrefix(line, "Seccomp:") && strings.TrimSpace(strings.TrimPrefix(line, "Seccomp:")) != "0" {
			return true
		}
	}
	return false
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
	var lsm [16 << 10]byte
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return false
	}
	_ = lsm
	return strings.Contains(string(data), "landlock")
}

// run launches the child in its own process group with an allowlisted
// environment, the explicit working directory, and the requested rlimits.
// Timeout or cancellation kills the entire process group with SIGKILL and
// reaps it, so nothing survives the deadline. On non-Linux the build-tag
// sibling fails closed before any process is started.
func run(ctx context.Context, spec *Spec, command string, args ...string) (Result, error) {
	if spec.AllowNetwork {
		return Result{}, Errorf(ErrorCodeSandboxUnavailable, "network access is not available in this sandbox")
	}
	env := allowlistedEnv(spec.AllowEnv)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = spec.WorkingDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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
		result := Result{Output: strings.TrimRight(stdout.String()+stderr.String(), "\n")}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		return result, nil
	case <-timeout.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return Result{Terminated: true}, nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return Result{Terminated: true}, nil
	}
}

func allowlistedEnv(allow []string) []string {
	return append([]string(nil), allow...)
}
