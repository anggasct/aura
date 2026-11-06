//go:build linux && !amd64

package sandbox

func seccompAvailable() bool { return false }

func applySeccomp(_ Profile) error {
	return Errorf(ErrorCodeSandboxUnavailable, "seccomp enforcement is not built for this architecture")
}
