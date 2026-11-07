package codeexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	runDirAttempts  = 8
	runDirNameBytes = 8
	reopenedDirMode = 0o700
)

type runDir struct {
	path   string
	name   string
	parent *os.Root
	lock   *os.File
}

func (d *runDir) outputDir() string {
	return filepath.Join(d.path, outputDirName)
}

func (d *runDir) release() {
	removeRunDir(d.parent, d.name)
	_ = d.lock.Close()
	_ = d.parent.Close()
}

func lockDir(dir *os.File) error {
	conn, err := dir.SyscallConn()
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

func (r *Runner) newRunDir() (*runDir, error) {
	if err := os.MkdirAll(r.runRoot, runDirMode); err != nil {
		return nil, wrapf(ErrorCodeRunFailed, pathFree(err), "create the run root")
	}
	parent, err := os.OpenRoot(r.runRoot)
	if err != nil {
		return nil, wrapf(ErrorCodeRunFailed, pathFree(err), "open the run root")
	}
	for range runDirAttempts {
		name, err := randomRunDirName()
		if err != nil {
			_ = parent.Close()
			return nil, err
		}
		if err := parent.Mkdir(name, runDirMode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			_ = parent.Close()
			return nil, wrapf(ErrorCodeRunFailed, pathFree(err), "create the run directory")
		}
		lock := lockRunDir(parent, name)
		if lock == nil {
			continue
		}
		dir := &runDir{path: filepath.Join(r.runRoot, name), name: name, parent: parent, lock: lock}
		if err := parent.Mkdir(name+"/"+outputDirName, outputDirMode); err != nil {
			dir.release()
			return nil, wrapf(ErrorCodeRunFailed, pathFree(err), "create the output directory")
		}
		return dir, nil
	}
	_ = parent.Close()
	return nil, Errorf(ErrorCodeRunFailed, "the run directory kept being swept before it was locked")
}

func randomRunDirName() (string, error) {
	var raw [runDirNameBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", wrapf(ErrorCodeRunFailed, err, "generate a run directory name")
	}
	return runDirPrefix + hex.EncodeToString(raw[:]), nil
}

func lockRunDir(parent *os.Root, name string) *os.File {
	handle, err := parent.Open(name)
	if err != nil {
		return nil
	}
	if lockDir(handle) != nil {
		_ = handle.Close()
		return nil
	}
	opened, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return nil
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(opened, current) {
		_ = handle.Close()
		return nil
	}
	return handle
}

func (r *Runner) SweepStale(ctx context.Context) (int, error) {
	parent, err := os.OpenRoot(r.runRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, wrapf(ErrorCodeRunFailed, pathFree(err), "open the run root")
	}
	defer func() { _ = parent.Close() }()
	entries, err := fs.ReadDir(parent.FS(), ".")
	if err != nil {
		return 0, wrapf(ErrorCodeRunFailed, pathFree(err), "read the run root")
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, wrapf(ErrorCodeRunFailed, err, "the sweep was cancelled")
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), runDirPrefix) {
			continue
		}
		if sweepRunDir(parent, entry.Name()) {
			removed++
		}
	}
	return removed, nil
}

func sweepRunDir(parent *os.Root, name string) bool {
	handle, err := parent.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = handle.Close() }()
	if lockDir(handle) != nil {
		return false
	}
	removeRunDir(parent, name)
	_, statErr := parent.Lstat(name)
	return errors.Is(statErr, fs.ErrNotExist)
}

func removeRunDir(parent *os.Root, name string) {
	if err := parent.RemoveAll(name); err == nil {
		return
	}
	if inner, err := parent.OpenRoot(name); err == nil {
		_ = fs.WalkDir(inner.FS(), ".", func(entry string, info fs.DirEntry, err error) error {
			if info != nil && info.IsDir() && entry != "." {
				_ = inner.Chmod(entry, reopenedDirMode)
			}
			return nil
		})
		_ = inner.Close()
	}
	_ = parent.RemoveAll(name)
}
