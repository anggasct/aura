package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Handler executes one tool after the broker has validated its grant. The
// handler itself never re-decides policy; it receives the decision's
// constraints and runs the tool under them.
type Handler func(ctx context.Context, request ToolRequest, constraints Constraints) (ToolResult, error)

// Engine is the canonical ToolBroker: every invocation is evaluated exactly
// once, and execution requires a grant bound to the evaluated request,
// current policy, and one-shot nonce.
type Engine struct {
	policy  Policy
	handler Handler
	now     func() time.Time
	mu      sync.Mutex
	nonces  map[string]time.Time // nonce -> expiry; consumed on Execute
}

// NewEngine builds an immutable-policy broker. The policy is loaded from
// trusted configuration only; nothing in a ToolRequest can alter it.
func NewEngine(policy Policy, handler Handler) (*Engine, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("approval: invalid policy: %w", err)
	}
	if handler == nil {
		return nil, errors.New("approval: handler must not be nil")
	}
	return &Engine{
		policy:  policy,
		handler: handler,
		now:     time.Now,
		nonces:  map[string]time.Time{},
	}, nil
}

func (e *Engine) PolicyVersion() string {
	return e.policy.Version
}

// Evaluate applies policy to a normalized structured request. Deny is the
// fail-closed default for unknown tools, disallowed trust labels, or
// missing capabilities. Untrusted and derived content is always data and
// can never alter policy; it can only be more restricted.
func (e *Engine) Evaluate(ctx context.Context, request *ToolRequest) (PolicyDecision, error) {
	decision, err := e.decide(ctx, request)
	if err != nil {
		return PolicyDecision{}, err
	}
	return decision, nil
}

func (e *Engine) decide(ctx context.Context, request *ToolRequest) (PolicyDecision, error) {
	if request == nil {
		return PolicyDecision{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if strings.TrimSpace(request.ToolName) == "" {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "tool name must not be empty")
	}
	if !request.Trust.Valid() {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "invalid trust label %q", request.Trust)
	}
	rule, ok := e.policy.Rules[request.ToolName]
	if !ok {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "unknown tool %q", request.ToolName)
	}
	if len(rule.AllowedTrust) > 0 && !slices.Contains(rule.AllowedTrust, request.Trust) {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "tool %q is not allowed for trust label %q", request.ToolName, request.Trust)
	}
	for _, required := range rule.RequiredCapabilities {
		if !slices.Contains(request.Capabilities, required) {
			return PolicyDecision{}, Errorf(ErrorCodeCapabilityUnavailable, "tool %q requires capability %q", request.ToolName, required)
		}
	}
	if err := ctx.Err(); err != nil {
		return PolicyDecision{}, err
	}
	if rule.RequiresApproval {
		return PolicyDecision{
			Outcome:       OutcomeRequireApproval,
			PolicyVersion: e.policy.Version,
			ReasonCode:    string(ErrorCodeApprovalRequired),
			Constraints:   rule.Constraints,
		}, nil
	}
	return PolicyDecision{
		Outcome:       OutcomeAllow,
		PolicyVersion: e.policy.Version,
		ReasonCode:    "allow",
		Constraints:   rule.Constraints,
	}, nil
}

// Grant mints a grant only after the request has passed policy, binding
// principal, session, tool, arguments hash, constraints, policy version,
// expiry, and a one-shot nonce. The decision is re-derived from policy, so
// a fabricated decision can never mint a grant; Execute is only reachable
// through this path. Changing any bound field later invalidates
// the grant.
func (e *Engine) Grant(ctx context.Context, request *ToolRequest, ttl time.Duration) (ApprovalGrant, error) {
	if request == nil {
		return ApprovalGrant{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	decision, err := e.decide(ctx, request)
	if err != nil {
		return ApprovalGrant{}, err
	}
	if ttl <= 0 {
		return ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "grant ttl must be positive")
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return ApprovalGrant{}, fmt.Errorf("approval: generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	grantIDBytes := make([]byte, 8)
	if _, err := rand.Read(grantIDBytes); err != nil {
		return ApprovalGrant{}, fmt.Errorf("approval: generate grant id: %w", err)
	}

	now := e.now()
	grant := ApprovalGrant{
		GrantID:          hex.EncodeToString(grantIDBytes),
		PrincipalID:      request.PrincipalID,
		SessionID:        request.SessionID,
		ToolName:         request.ToolName,
		ArgumentsHash:    HashArguments(request.Arguments),
		CapabilitiesHash: HashCapabilities(request.Capabilities),
		Constraints:      decision.Constraints,
		PolicyVersion:    decision.PolicyVersion,
		ExpiresAt:        now.Add(ttl),
		Nonce:            nonce,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruneNoncesLocked(now)
	e.nonces[nonce] = grant.ExpiresAt
	return grant, nil
}

// Execute runs the tool only when the grant is valid for the request and
// current policy, and the one-shot nonce has not been consumed. The handler
// receives the decision constraints and never re-decides policy.
func (e *Engine) Execute(ctx context.Context, request *ToolRequest, grant *ApprovalGrant) (ToolResult, error) {
	if request == nil {
		return ToolResult{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if grant == nil {
		return ToolResult{}, Errorf(ErrorCodeInvalidArgument, "grant must not be nil")
	}
	if err := grant.ValidFor(request, e.policy.Version, e.now()); err != nil {
		return ToolResult{}, err
	}
	if !e.consumeNonce(grant.Nonce) {
		return ToolResult{}, Errorf(ErrorCodeApprovalInvalid, "grant nonce was already consumed or is unknown")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	return e.handler(ctx, *request, grant.Constraints)
}

func (e *Engine) consumeNonce(nonce string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	expiry, ok := e.nonces[nonce]
	if !ok {
		return false
	}
	if e.now().After(expiry) {
		delete(e.nonces, nonce)
		return false
	}
	delete(e.nonces, nonce)
	return true
}

func (e *Engine) pruneNoncesLocked(now time.Time) {
	for nonce, expiry := range e.nonces {
		if now.After(expiry) {
			delete(e.nonces, nonce)
		}
	}
}

var _ ToolBroker = (*Engine)(nil)
