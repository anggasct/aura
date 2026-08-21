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
	if r.closing {
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
	if r.closing {
		r.mu.Unlock()
		return nil
	}
	r.closing = true
	sessions := make([]ProviderSession, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()

	var failures []error
	for _, session := range sessions {
		if err := session.Close(ctx); err != nil {
			failures = append(failures, err)
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, codedError(ErrorCodeShutdownTimeout, "provider session close timed out", err))
			break
		}
	}
	r.mu.Lock()
	clear(r.sessions)
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
