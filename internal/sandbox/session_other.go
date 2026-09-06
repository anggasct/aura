//go:build !linux

package sandbox

import "context"

// On non-Linux the containment contract cannot be enforced, so Start fails
// closed with sandbox_unavailable before any child process exists, exactly
// like the one-shot Run path. Sessions are never unconfined.
func startSession(_ context.Context, _ *SessionRequest) (*Session, error) {
	return nil, Errorf(ErrorCodeSandboxUnavailable, "sandbox requires Linux")
}
