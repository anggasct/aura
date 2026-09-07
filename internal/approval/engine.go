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

type Handler func(ctx context.Context, request ToolRequest, constraints Constraints) (ToolResult, error)

type Engine struct {
	policy  Policy
	handler Handler
	now     func() time.Time
	mu      sync.RWMutex
	nonces  map[string]time.Time // nonce -> expiry; consumed on Execute
}

func NewEngine(policy Policy, handler Handler) (*Engine, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("approval: invalid policy: %w", err)
	}
	if handler == nil {
		return nil, errors.New("approval: handler must not be nil")
	}
	clonedRules := make(map[string]Rule, len(policy.Rules))
	for k, v := range policy.Rules {
		clonedRules[k] = v
	}
	policyCopy := policy
	policyCopy.Rules = clonedRules
	return &Engine{
		policy:  policyCopy,
		handler: handler,
		now:     time.Now,
		nonces:  map[string]time.Time{},
	}, nil
}

func (e *Engine) PolicyVersion() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.policy.Version
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	return nil
}

func (e *Engine) Evaluate(ctx context.Context, request *ToolRequest) (PolicyDecision, error) {
	if err := validateContext(ctx); err != nil {
		return PolicyDecision{}, err
	}
	decision, err := e.decide(ctx, request)
	if err != nil {
		return PolicyDecision{}, err
	}
	return decision, nil
}

func (e *Engine) decide(ctx context.Context, request *ToolRequest) (PolicyDecision, error) {
	if err := validateContext(ctx); err != nil {
		return PolicyDecision{}, err
	}
	if request == nil {
		return PolicyDecision{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if strings.TrimSpace(request.ToolName) == "" {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "tool name must not be empty")
	}
	if !request.Trust.Valid() {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "invalid trust label %q", request.Trust)
	}
	e.mu.RLock()
	rule, ok := e.policy.Rules[request.ToolName]
	version := e.policy.Version
	e.mu.RUnlock()

	if !ok {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "unknown tool %q", request.ToolName)
	}
	if rule.ToolVersion != "" && request.ToolVersion != rule.ToolVersion {
		return PolicyDecision{}, Errorf(ErrorCodePolicyDenied, "tool %q version %q is not supported", request.ToolName, request.ToolVersion)
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
			PolicyVersion: version,
			ReasonCode:    string(ErrorCodeApprovalRequired),
			Constraints:   rule.Constraints,
		}, nil
	}
	return PolicyDecision{
		Outcome:       OutcomeAllow,
		PolicyVersion: version,
		ReasonCode:    "allow",
		Constraints:   rule.Constraints,
	}, nil
}

func (e *Engine) RegisterRule(rule *Rule) error {
	if rule == nil {
		return Errorf(ErrorCodeInvalidArgument, "rule must not be nil")
	}
	if strings.TrimSpace(rule.ToolName) == "" {
		return Errorf(ErrorCodeInvalidArgument, "rule tool name must not be empty")
	}
	for _, label := range rule.AllowedTrust {
		if !label.Valid() {
			return Errorf(ErrorCodeInvalidArgument, "rule %q has invalid trust label %q", rule.ToolName, label)
		}
	}
	for _, capability := range rule.RequiredCapabilities {
		if strings.TrimSpace(capability) == "" {
			return Errorf(ErrorCodeInvalidArgument, "rule %q has an empty required capability", rule.ToolName)
		}
	}
	if rule.Constraints.MaxOutputBytes < 0 || rule.Constraints.Timeout < 0 {
		return Errorf(ErrorCodeInvalidArgument, "rule %q has negative constraints", rule.ToolName)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.policy.Rules == nil {
		e.policy.Rules = make(map[string]Rule)
	}
	e.policy.Rules[rule.ToolName] = *rule
	return nil
}

func (e *Engine) UnregisterRule(toolName string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.policy.Rules, toolName)
}

func (e *Engine) Grant(ctx context.Context, request *ToolRequest, ttl time.Duration) (ApprovalGrant, error) {
	if err := validateContext(ctx); err != nil {
		return ApprovalGrant{}, err
	}
	if request == nil {
		return ApprovalGrant{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if ttl <= 0 {
		return ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "grant ttl must be positive")
	}
	return e.grantUntil(ctx, request, e.now().Add(ttl))
}

func (e *Engine) GrantUntil(ctx context.Context, request *ToolRequest, expiresAt time.Time) (ApprovalGrant, error) {
	if expiresAt.IsZero() {
		return ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "grant expiry must be set")
	}
	return e.grantUntil(ctx, request, expiresAt)
}

func (e *Engine) grantUntil(ctx context.Context, request *ToolRequest, expiresAt time.Time) (ApprovalGrant, error) {
	if err := validateContext(ctx); err != nil {
		return ApprovalGrant{}, err
	}
	if request == nil {
		return ApprovalGrant{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	now := e.now()
	if !now.Before(expiresAt) {
		return ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "grant expiry has passed")
	}
	decision, err := e.decide(ctx, request)
	if err != nil {
		return ApprovalGrant{}, err
	}
	now = e.now()
	if !now.Before(expiresAt) {
		return ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "grant expiry has passed")
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

	grant := ApprovalGrant{
		GrantID:          hex.EncodeToString(grantIDBytes),
		PrincipalID:      request.PrincipalID,
		SessionID:        request.SessionID,
		ToolName:         request.ToolName,
		ToolVersion:      request.ToolVersion,
		ArgumentsHash:    HashArguments(request.Arguments),
		RequestDigest:    request.RequestDigest,
		CapabilitiesHash: HashCapabilities(request.Capabilities),
		Constraints:      decision.Constraints,
		PolicyVersion:    decision.PolicyVersion,
		ExpiresAt:        expiresAt,
		Nonce:            nonce,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruneNoncesLocked(now)
	e.nonces[nonce] = grant.ExpiresAt
	return grant, nil
}

func (e *Engine) ValidateAndConsume(ctx context.Context, request *ToolRequest, grant *ApprovalGrant) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if request == nil {
		return Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if grant == nil {
		return Errorf(ErrorCodeInvalidArgument, "grant must not be nil")
	}
	decision, err := e.decide(ctx, request)
	if err != nil {
		return err
	}
	if err := grant.ValidForConstraints(request, e.policy.Version, e.now(), decision.Constraints); err != nil {
		return err
	}
	if !e.consumeNonce(grant.Nonce) {
		return Errorf(ErrorCodeApprovalInvalid, "grant nonce was already consumed or is unknown")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (e *Engine) Execute(ctx context.Context, request *ToolRequest, grant *ApprovalGrant) (ToolResult, error) {
	if err := e.ValidateAndConsume(ctx, request, grant); err != nil {
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
	if !e.now().Before(expiry) {
		delete(e.nonces, nonce)
		return false
	}
	delete(e.nonces, nonce)
	return true
}

func (e *Engine) pruneNoncesLocked(now time.Time) {
	for nonce, expiry := range e.nonces {
		if !now.Before(expiry) {
			delete(e.nonces, nonce)
		}
	}
}

var _ ToolBroker = (*Engine)(nil)
