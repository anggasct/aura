//go:build !linux && !darwin

package interpreter

import "os"

type installLock struct{}

func acquireInstallLock(*os.Root) (*installLock, error) {
	return nil, Errorf(ErrorCodeUnsupportedPlatform, "runtime installs are not supported on this platform")
}

func (*installLock) release() {}
