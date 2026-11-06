package approval

import (
	"context"
	"strings"
	"sync"
)

const maxRemembered = 4096

type OwnerApprover interface {
	DecideForOwner(ctx context.Context, prompt *Prompt, requester *Requester) (bool, error)
}

type heldRequester struct {
	requester  Requester
	turns      int
	remembered bool
}

type OwnerRoute struct {
	owners     OwnerApprover
	mu         sync.Mutex
	held       map[string]*heldRequester
	remembered int
}

func NewOwnerRoute(owners OwnerApprover) (*OwnerRoute, error) {
	if owners == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "owner approver must not be nil")
	}
	return &OwnerRoute{owners: owners, held: make(map[string]*heldRequester)}, nil
}

func (r *OwnerRoute) HoldsApprovalTarget(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.held[sessionID]
	return ok
}

func (r *OwnerRoute) Decide(ctx context.Context, prompt *Prompt) (bool, error) {
	if ctx == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
	}
	requester, ok := r.RequesterOf(prompt.SessionID)
	if !ok || requester.Principal != prompt.PrincipalID {
		return false, nil
	}
	return r.owners.DecideForOwner(ctx, prompt, &requester)
}

func (r *OwnerRoute) RequesterOf(sessionID string) (Requester, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.held[sessionID]
	if !ok {
		return Requester{}, false
	}
	return entry.requester, true
}

func (r *OwnerRoute) Hold(sessionID string, requester *Requester) (release func()) {
	if r == nil || requester == nil {
		return func() {}
	}
	r.mu.Lock()
	entry, ok := r.held[sessionID]
	if !ok {
		entry = &heldRequester{requester: *requester}
		r.held[sessionID] = entry
	}
	entry.turns++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			entry.turns--
			if entry.turns == 0 && !entry.remembered {
				delete(r.held, sessionID)
			}
		})
	}
}

func (r *OwnerRoute) Remember(sessionID string, requester *Requester) {
	if r == nil || requester == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.held[sessionID]; ok {
		if !entry.remembered {
			entry.remembered = true
			r.remembered++
		}
		return
	}
	if r.remembered >= maxRemembered {
		r.evictRemembered()
	}
	r.held[sessionID] = &heldRequester{requester: *requester, remembered: true}
	r.remembered++
}

func (r *OwnerRoute) evictRemembered() {
	for id, entry := range r.held {
		if entry.remembered && entry.turns == 0 {
			delete(r.held, id)
			r.remembered--
			return
		}
	}
}
