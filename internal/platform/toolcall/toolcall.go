package toolcall

import (
	"context"
	"fmt"
)

type Batch struct {
	slots []*Slot
}

type Slot struct {
	number int
	index  int
}

func NewBatch(number, size int) *Batch {
	slots := make([]*Slot, max(size, 0))
	for i := range slots {
		slots[i] = &Slot{number: number, index: i}
	}
	return &Batch{slots: slots}
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
