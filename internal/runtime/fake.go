package runtime

import (
	"context"
	"encoding/json"
	"iter"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/store"
)

// FakeExecutor is the deterministic turn executor used by tests: it replays
// a scripted event stream and records every start so tests can assert
// ordering and concurrency. It replaces the ADK runner in step 1 until that
// adapter exists.
type FakeExecutor struct {
	script []FakeStep
	mu     sync.Mutex
	starts []startRecord
}

// FakeStep is one scripted executor action.
type FakeStep struct {
	// Kind, when non-empty, emits an event with this kind and payload.
	Kind    string
	Payload json.RawMessage
	// Block, when non-nil, waits until the channel closes or the context
	// ends before the next step.
	Block <-chan struct{}
	// Wait is a wall-clock pause before the next step.
	Wait time.Duration
}

type startRecord struct {
	turnID    string
	startedAt time.Time
}

// NewFakeExecutor returns an executor that replays script on every turn.
func NewFakeExecutor(script []FakeStep) *FakeExecutor {
	return &FakeExecutor{script: script}
}

// Execute replays the script and records the start.
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
					ID:            req.TurnID + "-step",
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

// StartCount reports how many turns the executor has started.
func (f *FakeExecutor) StartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// StartOrder returns the turn IDs in start order.
func (f *FakeExecutor) StartOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	order := make([]string, len(f.starts))
	for i, s := range f.starts {
		order[i] = s.turnID
	}
	return order
}

// StartedAt returns the recorded start time of the n-th started turn.
func (f *FakeExecutor) StartedAt(n int) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 0 || n >= len(f.starts) {
		return time.Time{}
	}
	return f.starts[n].startedAt
}

// CompletedAt reports when the n-th started turn's fake work would have
// finished: after its script's total wait time.
func (f *FakeExecutor) CompletedAt(n int) time.Time {
	total := time.Duration(0)
	for _, step := range f.script {
		total += step.Wait
	}
	return f.StartedAt(n).Add(total)
}
