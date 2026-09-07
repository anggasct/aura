//go:build !linux

package sandbox

import "context"

func negotiate() (Primitives, error) {
	return Primitives{}, Errorf(ErrorCodeSandboxUnavailable, "sandbox requires Linux")
}

func run(_ context.Context, _ *Spec, _ Primitives, _ string, _ ...string) (Result, error) {
	return Result{}, Errorf(ErrorCodeSandboxUnavailable, "sandbox requires Linux")
}
