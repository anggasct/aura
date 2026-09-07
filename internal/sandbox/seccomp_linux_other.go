//go:build linux && !amd64

package sandbox

func seccompAvailable() bool { return false }

func applySeccomp() error {
	return Errorf(ErrorCodeSandboxUnavailable, "seccomp enforcement is not built for this architecture")
}
