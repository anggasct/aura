package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type ProviderSession = ShutdownResource

type SessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]ProviderSession
	closing  bool
	failed   bool
	closed   bool
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{sessions: make(map[string]ProviderSession)}
}

func (r *SessionRegistry) Register(id string, session ProviderSession) error {
	if r == nil || id == "" || session == nil {
		return invalidArgument("provider session registration is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || r.failed || r.closed {
		return codedError(ErrorCodeShutdownTimeout, "provider session registry is closing", nil)
	}
	if _, exists := r.sessions[id]; exists {
		return codedError(ErrorCodeCatalogInvalid, "provider session is already registered", nil)
	}
	r.sessions[id] = session
	return nil
}

func (r *SessionRegistry) Unregister(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

func (r *SessionRegistry) Close(ctx context.Context) error {
	if r == nil || ctx == nil {
		return invalidArgument("session registry and context must not be nil")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if r.closing {
		r.mu.Unlock()
		return codedError(ErrorCodeShutdownTimeout, "provider session registry is already closing", nil)
	}
	r.closing = true
	sessions := make(map[string]ProviderSession, len(r.sessions))
	for id, session := range r.sessions {
		sessions[id] = session
	}
	r.mu.Unlock()

	var failures []error
	closed := make([]string, 0, len(sessions))
	for id, session := range sessions {
		if err := session.Close(ctx); err != nil {
			failures = append(failures, err)
		} else {
			closed = append(closed, id)
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, codedError(ErrorCodeShutdownTimeout, "provider session close timed out", err))
			break
		}
	}
	r.mu.Lock()
	for _, id := range closed {
		delete(r.sessions, id)
	}
	r.failed = len(failures) > 0
	r.closed = len(failures) == 0 && len(r.sessions) == 0
	r.closing = false
	r.mu.Unlock()
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func (r *SessionRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *SessionRegistry) String() string {
	return fmt.Sprintf("provider sessions: %d", r.Count())
}
