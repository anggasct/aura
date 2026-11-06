package interpreter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const treeDigestVersion = "aura-interpreter-tree-v1"

type treeProblem struct {
	reason Reason
}

func (p *treeProblem) Error() string {
	return "bundle tree is not acceptable: " + string(p.reason)
}

func treeDigest(ctx context.Context, bundle *os.Root, runtime *Runtime, limits extractLimits) (string, error) {
	outer := sha256.New()
	_, _ = fmt.Fprintf(outer, "%s\n%s\n%s\n%s\n", treeDigestVersion, runtime.ID, runtime.Version, runtime.SHA256)
	entries := 0
	var total int64
	walkErr := fs.WalkDir(bundle.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return &treeProblem{reason: ReasonTreeUnreadable}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > limits.maxEntries+1 {
			return &treeProblem{reason: ReasonTreeUnsafe}
		}
		info, err := entry.Info()
		if err != nil {
			return &treeProblem{reason: ReasonTreeUnreadable}
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			_, _ = fmt.Fprintf(outer, "d %s %d:%s -\n", mode, len(name), name)
		case mode.IsRegular():
			total += info.Size()
			if total > limits.maxExpanded {
				return &treeProblem{reason: ReasonTreeUnsafe}
			}
			sum, err := hashFile(ctx, bundle, name)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(outer, "f %s %d:%s %d %s\n", mode, len(name), name, info.Size(), sum)
		case mode&fs.ModeSymlink != 0:
			target, err := bundle.Readlink(name)
			if err != nil {
				return &treeProblem{reason: ReasonTreeUnreadable}
			}
			_, _ = fmt.Fprintf(outer, "l %d:%s %d:%s\n", len(name), name, len(target), target)
		default:
			return &treeProblem{reason: ReasonTreeUnsafe}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	return hex.EncodeToString(outer.Sum(nil)), nil
}

func hashFile(ctx context.Context, bundle *os.Root, name string) (string, error) {
	file, err := bundle.Open(name)
	if err != nil {
		return "", &treeProblem{reason: ReasonTreeUnreadable}
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := copyWithContext(ctx, hash, file); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", &treeProblem{reason: ReasonTreeUnreadable}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func asTreeReason(err error) (Reason, bool) {
	var problem *treeProblem
	if errors.As(err, &problem) {
		return problem.reason, true
	}
	return "", false
}
