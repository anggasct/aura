//go:build linux && !amd64

package sandbox

// seccompProfileVersion mirrors the amd64 profile version so telemetry code
// compiles uniformly; on non-amd64 Linux the filter itself is not compiled.
const seccompProfileVersion = "2026.08"

// seccompAvailable reports no enforcement on non-amd64 Linux: the curated
// allowlist is amd64-specific, so Require fails closed here rather than
// installing an arch-mismatched filter.
func seccompAvailable() bool { return false }

// applySeccomp is unreachable while seccompAvailable reports false (Require
// refuses to advertise the capability), but returns a typed error if ever
// called directly so no caller proceeds unfiltered.
func applySeccomp() error {
	return Errorf(ErrorCodeSandboxUnavailable, "seccomp enforcement is not built for this architecture")
}
