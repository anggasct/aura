package restate

import (
	"fmt"
	"sync"

	"github.com/anggasct/aura/internal/durable"
)

type handlerRegistry struct {
	mu       sync.Mutex
	handlers map[string]durable.Handler
}

func newHandlerRegistry() *handlerRegistry {
	return &handlerRegistry{handlers: map[string]durable.Handler{}}
}

func (r *handlerRegistry) register(name string, fn durable.Handler) {
	if name == "" {
		panic("restate adapter requires a handler name")
	}
	if fn == nil {
		panic("restate adapter requires a handler function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, duplicate := r.handlers[name]; duplicate {
		panic(fmt.Sprintf("restate adapter already has a handler for %q", name))
	}
	r.handlers[name] = fn
}

func (r *handlerRegistry) get(name string) (durable.Handler, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, ok := r.handlers[name]
	return fn, ok
}

func (r *handlerRegistry) snapshot() map[string]durable.Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	handlers := make(map[string]durable.Handler, len(r.handlers))
	for name, fn := range r.handlers {
		handlers[name] = fn
	}
	return handlers
}
