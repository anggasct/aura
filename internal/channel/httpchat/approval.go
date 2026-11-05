package httpchat

import (
	"context"
	"strings"
	"sync"

	"github.com/anggasct/aura/internal/channel/approval"
)

type OwnerApprover interface {
	DecideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approval.Requester) (bool, error)
}

type heldRequester struct {
	requester approval.Requester
	turns     int
}

type Approvals struct {
	owners OwnerApprover
	mu     sync.Mutex
	held   map[string]*heldRequester
}

func NewApprovals(owners OwnerApprover) (*Approvals, error) {
	if owners == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "owner approver must not be nil")
	}
	return &Approvals{owners: owners, held: make(map[string]*heldRequester)}, nil
}

func (a *Approvals) HoldsApprovalTarget(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.held[sessionID]
	return ok
}

func (a *Approvals) Decide(ctx context.Context, prompt *approval.Prompt) (bool, error) {
	if ctx == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
	}
	requester, ok := a.requesterOf(prompt.SessionID)
	if !ok || requester.Principal != prompt.PrincipalID {
		return false, nil
	}
	return a.owners.DecideForOwner(ctx, prompt, &requester)
}

func (a *Approvals) requesterOf(sessionID string) (approval.Requester, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.held[sessionID]
	if !ok {
		return approval.Requester{}, false
	}
	return entry.requester, true
}

func (a *Approvals) hold(sessionID string, requester *approval.Requester) (release func()) {
	if a == nil {
		return func() {}
	}
	a.mu.Lock()
	entry, ok := a.held[sessionID]
	if !ok {
		entry = &heldRequester{requester: *requester}
		a.held[sessionID] = entry
	}
	entry.turns++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			entry.turns--
			if entry.turns == 0 {
				delete(a.held, sessionID)
			}
		})
	}
}
