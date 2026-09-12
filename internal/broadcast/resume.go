package broadcast

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/durable"
)

const resumeBatchLimit = 512

type ResumeStore interface {
	ListActive(ctx context.Context, limit int) ([]FullItem, error)
}

func StartItemRun(ctx context.Context, runtime durable.Runtime, itemID string) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if runtime == nil {
		return Errorf(ErrorCodeInvalidArgument, "durable runtime must not be nil")
	}
	if strings.TrimSpace(itemID) == "" {
		return Errorf(ErrorCodeInvalidArgument, "broadcast item id must not be empty")
	}
	_, err := runtime.Start(ctx, durable.StartRequest{
		Handler: HandlerName,
		Key:     itemID,
		Payload: []byte(itemID),
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
		if err := StartItemRun(ctx, runtime, active[i].ID); err != nil {
			return 0, err
		}
	}
	return len(active), nil
}
