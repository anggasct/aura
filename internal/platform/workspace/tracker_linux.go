//go:build linux

package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func uint64ToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func coarseNow() time.Time {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME_COARSE, &ts); err == nil {
		return time.Unix(ts.Sec, ts.Nsec).UTC()
	}
	return time.Now().UTC()
}

func statAndHashWorkspaceFile(rootPath, relative string) (sha256Hex string, device, inode int64, err error) {
	file, err := OpenRead(rootPath, relative)
	if err != nil {
		return "", 0, 0, err
	}
	defer func() { _ = file.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return "", 0, 0, fmt.Errorf("workspace: stat file %s: %w", relative, err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", 0, 0, fmt.Errorf("workspace: hash file %s: %w", relative, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), uint64ToInt64(st.Dev), uint64ToInt64(st.Ino), nil
}

func walkWorkspace(ctx context.Context, rootPath string, since time.Time, maxEntries int) (WalkResult, error) {
	rootFD, err := OpenRoot(rootPath)
	if err != nil {
		return WalkResult{}, fmt.Errorf("workspace: open root: %w", err)
	}
	defer func() { _ = unix.Close(rootFD) }()

	var result WalkResult
	seen := 0
	var walkDir func(rel string) error
	walkDir = func(rel string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fd, err := OpenBeneath(rootFD, rel, uint64(unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC), 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return err
		}
		dir := os.NewFile(uintptr(fd), "walk-dir")
		if dir == nil {
			_ = unix.Close(fd)
			return errors.New("workspace: directory handle unavailable")
		}
		entries, err := dir.ReadDir(-1)
		_ = dir.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			seen++
			if seen > maxEntries {
				result.ExceededBound = true
				return nil
			}
			name := entry.Name()
			childRel := name
			if rel != "." {
				childRel = filepath.ToSlash(filepath.Join(rel, name))
			}
			typ := entry.Type()
			if typ&os.ModeSymlink != 0 {
				continue
			}
			if entry.IsDir() {
				if err := walkDir(childRel); err != nil {
					return err
				}
				if result.ExceededBound {
					return nil
				}
				continue
			}
			if !typ.IsRegular() {
				continue
			}
			wf, ok, err := inspectRegularFile(rootFD, childRel, since)
			if err != nil {
				return err
			}
			if ok {
				result.Files = append(result.Files, wf)
			}
		}
		return nil
	}

	if err := walkDir("."); err != nil {
		return WalkResult{}, err
	}
	return result, nil
}

func inspectRegularFile(rootFD int, relative string, since time.Time) (WalkedFile, bool, error) {
	fd, err := OpenBeneath(rootFD, relative, uint64(unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK), 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || Escapes(err) {
			return WalkedFile{}, false, nil
		}
		return WalkedFile{}, false, err
	}
	file := os.NewFile(uintptr(fd), "walk-file")
	if file == nil {
		_ = unix.Close(fd)
		return WalkedFile{}, false, nil
	}
	defer func() { _ = file.Close() }()

	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return WalkedFile{}, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return WalkedFile{}, false, nil
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return WalkedFile{}, false, err
	}
	ctime := time.Unix(st.Ctim.Sec, st.Ctim.Nsec).UTC()
	changed := !ctime.Before(since)
	return WalkedFile{
		Path:          relative,
		ContentSHA256: hex.EncodeToString(hasher.Sum(nil)),
		Device:        uint64ToInt64(st.Dev),
		Inode:         uint64ToInt64(st.Ino),
		Changed:       changed,
	}, true, nil
}
