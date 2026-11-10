//go:build !linux

package sandbox

import "context"

func startSession(_ context.Context, _ *SessionRequest, _ *Spec) (*Session, error) {
	return nil, Errorf(ErrorCodeSandboxUnavailable, "sandbox requires Linux")
}
