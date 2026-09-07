package runtime

import (
	"context"
	"encoding/json"
	"iter"
	"strconv"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/store"
)

type FakeExecutor struct {
	script []FakeStep
	mu     sync.Mutex
	starts []startRecord
}

type FakeStep struct {
	Kind    string
	Payload json.RawMessage
	Block   <-chan struct{}
	Wait    time.Duration
}

type startRecord struct {
	turnID    string
	startedAt time.Time
}

func NewFakeExecutor(script []FakeStep) *FakeExecutor {
	return &FakeExecutor{script: script}
}

func (f *FakeExecutor) Execute(ctx context.Context, req *TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		f.mu.Lock()
		f.starts = append(f.starts, startRecord{turnID: req.TurnID, startedAt: time.Now()})
		f.mu.Unlock()

		seq := uint64(0)
		for _, step := range f.script {
			if err := ctx.Err(); err != nil {
				yield(store.RuntimeEvent{}, err)
				return
			}
			if step.Block != nil {
				select {
				case <-step.Block:
				case <-ctx.Done():
					yield(store.RuntimeEvent{}, ctx.Err())
					return
				}
			}
			if step.Wait > 0 {
				select {
				case <-time.After(step.Wait):
				case <-ctx.Done():
					yield(store.RuntimeEvent{}, ctx.Err())
					return
				}
			}
			if step.Kind != "" {
				seq++
				if !yield(store.RuntimeEvent{
					ID:            req.TurnID + "-step-" + strconv.FormatUint(seq, 10),
					Sequence:      seq,
					TurnID:        req.TurnID,
					Kind:          step.Kind,
					SchemaVersion: 1,
					Payload:       step.Payload,
					CreatedAt:     time.Now().UTC(),
				}, nil) {
					return
				}
			}
		}
	}
}

func (f *FakeExecutor) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func (f *FakeExecutor) StartOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	order := make([]string, len(f.starts))
	for i, s := range f.starts {
		order[i] = s.turnID
	}
	return order
}

func (f *FakeExecutor) StartedAt(n int) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 0 || n >= len(f.starts) {
		return time.Time{}
	}
	return f.starts[n].startedAt
}

func (f *FakeExecutor) CompletedAt(n int) time.Time {
	total := time.Duration(0)
	for _, step := range f.script {
		total += step.Wait
	}
	return f.StartedAt(n).Add(total)
}
