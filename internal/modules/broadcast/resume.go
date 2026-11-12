package broadcast

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/platform/durable"
)

const resumeBatchLimit = 512

type ResumeStore interface {
	ListActive(ctx context.Context, limit int) ([]FullItem, error)
}

type itemRun struct {
	runtime durable.Runtime
	reuse   durable.ReusePolicy
}

func StartItemRun(ctx context.Context, runtime durable.Runtime, itemID string) error {
	return itemRun{runtime: runtime}.start(ctx, itemID)
}

func ResumeItemRun(ctx context.Context, runtime durable.Runtime, itemID string) error {
	return itemRun{runtime: runtime, reuse: durable.ReuseEnded}.start(ctx, itemID)
}

func (r itemRun) start(ctx context.Context, itemID string) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if r.runtime == nil {
		return Errorf(ErrorCodeInvalidArgument, "durable runtime must not be nil")
	}
	if strings.TrimSpace(itemID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "broadcast item id must not be empty")
	}
	_, err := r.runtime.Start(ctx, durable.StartRequest{
		Handler: HandlerName,
		Key:     itemID,
		Payload: []byte(itemID),
		Reuse:   r.reuse,
	})
	return err
}

func ResumeActive(ctx context.Context, runtime durable.Runtime, items ResumeStore) (int, error) {
	if ctx == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if runtime == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "durable runtime must not be nil")
	}
	if items == nil {
		return 0, Errorf(ErrorCodeInvalidArgument, "resume store must not be nil")
	}
	active, err := items.ListActive(ctx, resumeBatchLimit)
	if err != nil {
		return 0, err
	}
	for i := range active {
		if err := ResumeItemRun(ctx, runtime, active[i].ID); err != nil {
			return 0, err
		}
	}
	return len(active), nil
}
