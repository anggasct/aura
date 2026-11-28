//go:build !linux

package workspace

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

func coarseNow() time.Time {
	return time.Now().UTC()
}

func statAndHashWorkspaceFile(_, _ string) (digest string, device, inode int64, err error) {
	return "", 0, 0, fault.New(ErrorCodeOutside, "workspace provenance requires Linux")
}

func walkWorkspace(_ context.Context, _ string, _ time.Time, _ int) (WalkResult, error) {
	return WalkResult{ExceededBound: true}, nil
}
