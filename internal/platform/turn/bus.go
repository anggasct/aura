package turn

import (
	"context"
	"log/slog"
	"sync"

	"github.com/anggasct/aura/internal/store"
)

type EventBus struct {
	logger   *slog.Logger
	mu       sync.RWMutex
	handlers []func(*store.RuntimeEvent)
}

func NewEventBus(logger *slog.Logger) *EventBus {
	if logger == nil {
		logger = slog.Default()
	}
	return &EventBus{logger: logger}
}

func (b *EventBus) Subscribe(handler func(*store.RuntimeEvent)) error {
	if handler == nil {
		return invalidArgument("event subscriber must not be nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers = append(b.handlers, handler)
	return nil
}

func (b *EventBus) Publish(ev *store.RuntimeEvent) {
	if ev == nil {
		return
	}
	b.mu.RLock()
	handlers := make([]func(*store.RuntimeEvent), len(b.handlers))
	copy(handlers, b.handlers)
	b.mu.RUnlock()
	for _, handler := range handlers {
		b.deliver(handler, ev)
	}
}

func (b *EventBus) deliver(handler func(*store.RuntimeEvent), ev *store.RuntimeEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			b.logger.ErrorContext(context.Background(), "live event subscriber panicked",
				"kind", ev.Kind,
				"session_id", ev.SessionID,
				"turn_id", ev.TurnID,
				"panic", recovered,
			)
		}
	}()
	handler(ev)
}
