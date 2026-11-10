//go:build linux || darwin

package interpreter

import (
	"errors"
	"os"
	"syscall"
)

type installLock struct {
	file *os.File
}

func acquireInstallLock(dir *os.Root) (*installLock, error) {
	file, err := dir.OpenFile(".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ioError("lock runtimes directory", err)
	}
	if err := flock(file); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, Errorf(ErrorCodeInstallBusy, "another runtime install or removal is running")
		}
		return nil, ioError("lock runtimes directory", err)
	}
	return &installLock{file: file}, nil
}

func flock(file *os.File) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := conn.Control(func(fd uintptr) {
		lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	}); err != nil {
		return err
	}
	return lockErr
}

func (l *installLock) release() {
	_ = l.file.Close()
}
