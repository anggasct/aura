//go:build !linux

package sandbox

import "context"

// On non-Linux the containment contract cannot be enforced, so every run
// fails closed with sandbox_unavailable before any child process exists.
// Effectful capabilities are reported unavailable by the capability
// registry on these builds as well, so this path is never reachable from
// an effectful tool.

func negotiate() (Primitives, error) {
	return Primitives{}, Errorf(ErrorCodeSandboxUnavailable, "sandbox requires Linux")
}

func run(_ context.Context, _ *Spec, _ string, _ ...string) (Result, error) {
	return Result{}, Errorf(ErrorCodeSandboxUnavailable, "sandbox requires Linux")
}
