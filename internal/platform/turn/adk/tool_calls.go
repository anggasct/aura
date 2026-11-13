package runtimeadk

import (
	"context"
	"sync/atomic"

	"github.com/anggasct/aura/internal/platform/toolcall"
)

type toolCallRunner struct {
	batches atomic.Int64
}

func newToolCallRunner() *toolCallRunner {
	return &toolCallRunner{}
}

func (r *toolCallRunner) run(ctx context.Context, tasks []func(context.Context)) {
	batch := toolcall.NewBatch(int(r.batches.Add(1)), len(tasks))
	for i, task := range tasks {
		task(toolcall.With(ctx, batch.Slot(i)))
	}
}
