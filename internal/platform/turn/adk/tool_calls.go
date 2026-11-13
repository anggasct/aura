package runtimeadk

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/toolcall"
)

const maxParallelToolCalls = 8

type toolCallPanic struct {
	value any
	stack []byte
}

func (p *toolCallPanic) String() string {
	return fmt.Sprintf("tool call panicked: %v\n%s", p.value, p.stack)
}

type toolCallRunner struct {
	batches atomic.Int64
}

func newToolCallRunner() *toolCallRunner {
	return &toolCallRunner{}
}

func (r *toolCallRunner) run(ctx context.Context, tasks []func(context.Context)) {
	batch := toolcall.NewBatch(int(r.batches.Add(1)), len(tasks))
	if scope, scoped := durable.TurnScopeFrom(ctx); scoped && !durable.ConcurrentActions(scope.Invocation()) {
		runInline(ctx, batch, tasks)
		return
	}
	runConcurrent(ctx, batch, tasks)
}

func runInline(ctx context.Context, batch *toolcall.Batch, tasks []func(context.Context)) {
	for i, task := range tasks {
		slot := batch.Slot(i)
		func() {
			defer slot.Close()
			task(toolcall.With(ctx, slot))
		}()
	}
}

func runConcurrent(ctx context.Context, batch *toolcall.Batch, tasks []func(context.Context)) {
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		panicked *toolCallPanic
	)
	running := make(chan struct{}, maxParallelToolCalls)
	for i, task := range tasks {
		running <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-running }()
			slot := batch.Slot(i)
			if raised := callTask(toolcall.With(batchCtx, slot), slot, task); raised != nil {
				mu.Lock()
				if panicked == nil {
					panicked = raised
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
}

func callTask(ctx context.Context, slot *toolcall.Slot, task func(context.Context)) (raised *toolCallPanic) {
	defer func() {
		if value := recover(); value != nil {
			raised = &toolCallPanic{value: value, stack: debug.Stack()}
		}
	}()
	defer slot.Close()
	task(ctx)
	return nil
}
