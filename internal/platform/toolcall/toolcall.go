package toolcall

import (
	"context"
	"fmt"
	"sync"
)

type Batch struct {
	slots []*Slot
}

type Slot struct {
	batch      *Batch
	number     int
	index      int
	alone      bool
	authorize  sync.Once
	authorized chan struct{}
	closing    sync.Once
	closed     chan struct{}
}

func NewBatch(number, size int) *Batch {
	batch := &Batch{slots: make([]*Slot, max(size, 0))}
	for i := range batch.slots {
		batch.slots[i] = &Slot{batch: batch, number: number, index: i, authorized: make(chan struct{}), closed: make(chan struct{})}
	}
	return batch
}

func (b *Batch) Slot(i int) *Slot {
	if b == nil || i < 0 || i >= len(b.slots) {
		return nil
	}
	return b.slots[i]
}

func (s *Slot) Key() string {
	return fmt.Sprintf("call-%d-%d", s.number, s.index)
}

func (s *Slot) Action(name string) string {
	return s.Key() + "/" + name
}

type slotKey struct{}

func With(ctx context.Context, slot *Slot) context.Context {
	if slot == nil {
		return ctx
	}
	return context.WithValue(ctx, slotKey{}, slot)
}

func From(ctx context.Context) (*Slot, bool) {
	slot, ok := ctx.Value(slotKey{}).(*Slot)
	if !ok || slot == nil {
		return nil, false
	}
	return slot, true
}

func (s *Slot) AwaitAuthorization(ctx context.Context) error {
	for _, earlier := range s.batch.slots[:s.index] {
		if err := waitFor(ctx, earlier.authorized); err != nil {
			return err
		}
	}
	return nil
}

func (s *Slot) Authorized(alone bool) {
	s.authorize.Do(func() {
		s.alone = alone
		close(s.authorized)
	})
}

func (s *Slot) AwaitExecution(ctx context.Context) error {
	for _, earlier := range s.batch.slots[:s.index] {
		if err := waitFor(ctx, earlier.authorized); err != nil {
			return err
		}
		if !s.alone && !earlier.alone {
			continue
		}
		if err := waitFor(ctx, earlier.closed); err != nil {
			return err
		}
	}
	return nil
}

func (s *Slot) Close() {
	s.Authorized(true)
	s.closing.Do(func() { close(s.closed) })
}

func waitFor(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
