package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// TrustLabel classifies the origin of an input. Only trusted configuration
// may define policy; untrusted and derived content is always data.
type TrustLabel string

const (
	TrustOwnerInput           TrustLabel = "owner_input"
	TrustTrustedConfiguration TrustLabel = "trusted_configuration"
	TrustUntrustedExternal    TrustLabel = "untrusted_external"
	TrustDerivedUntrusted     TrustLabel = "derived_untrusted"
)

func (t TrustLabel) Valid() bool {
	switch t {
	case TrustOwnerInput, TrustTrustedConfiguration, TrustUntrustedExternal, TrustDerivedUntrusted:
		return true
	}
	return false
}

// IsUntrusted reports whether content with this label may never become a
// system instruction or policy rule.
func (t TrustLabel) IsUntrusted() bool {
	return t == TrustUntrustedExternal || t == TrustDerivedUntrusted
}

// ToolRequest is the normalized structured request evaluated by policy.
// Authorization is decided on this structure, never on model-generated
// prose.
type ToolRequest struct {
	RequestID      string
	TurnID         string
	SessionID      string
	PrincipalID    string
	ToolName       string
	ToolVersion    string
	Arguments      json.RawMessage
	ArgumentsHash  string
	RequestDigest  string
	Capabilities   []string
	Trust          TrustLabel
	Deadline       time.Time
	IdempotencyKey string
}

type Constraints struct {
	AllowNetwork   bool
	MaxOutputBytes int64
	Timeout        time.Duration
}

type PolicyDecision struct {
	Outcome       string // allow, deny, require_approval
	PolicyVersion string
	ReasonCode    string
	Constraints   Constraints
}

const (
	OutcomeAllow           = "allow"
	OutcomeDeny            = "deny"
	OutcomeRequireApproval = "require_approval"
)

type ToolResult struct {
	ToolName  string
	Output    json.RawMessage
	Truncated bool
}

// ToolBroker is the single decision point every tool invocation passes
// through before execution. A grant obtained from Evaluate is the only way
// to reach Execute. Pointer parameters are required; a nil argument returns
// invalid_argument rather than panicking.
type ToolBroker interface {
	Evaluate(ctx context.Context, request *ToolRequest) (PolicyDecision, error)
	Execute(ctx context.Context, request *ToolRequest, grant *ApprovalGrant) (ToolResult, error)
}

// Rule is the policy for one tool. AllowedTrust restricts which input trust
// labels may invoke the tool; empty means any valid label. Untrusted labels
// are always honored as constraints, never as policy.
type Rule struct {
	ToolName             string
	ToolVersion          string
	RequiresApproval     bool
	RequiredCapabilities []string
	AllowedTrust         []TrustLabel
	Constraints          Constraints
}

// Policy is immutable after construction. It is loaded from trusted
// configuration only; nothing in a ToolRequest can modify it.
type Policy struct {
	Version string
	Rules   map[string]Rule
}

func (p Policy) Validate() error {
	var problems []error
	if strings.TrimSpace(p.Version) == "" {
		problems = append(problems, errors.New("policy version must not be empty"))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Rules)) {
		rule := p.Rules[name]
		if strings.TrimSpace(rule.ToolName) != name || strings.TrimSpace(name) == "" {
			problems = append(problems, fmt.Errorf("policy rule tool name %q does not match its key %q", rule.ToolName, name))
		}
		for _, label := range rule.AllowedTrust {
			if !label.Valid() {
				problems = append(problems, fmt.Errorf("policy rule %q has invalid trust label %q", name, label))
			}
		}
		for _, capability := range rule.RequiredCapabilities {
			if strings.TrimSpace(capability) == "" {
				problems = append(problems, fmt.Errorf("policy rule %q has an empty required capability", name))
			}
		}
		if rule.Constraints.MaxOutputBytes < 0 || rule.Constraints.Timeout < 0 {
			problems = append(problems, fmt.Errorf("policy rule %q has negative constraints", name))
		}
	}
	return errors.Join(problems...)
}

// ApprovalGrant binds everything the execution depends on. Changing any
// bound field invalidates the grant.
type ApprovalGrant struct {
	GrantID          string
	PrincipalID      string
	SessionID        string
	ToolName         string
	ToolVersion      string
	ArgumentsHash    string
	RequestDigest    string
	CapabilitiesHash string
	Constraints      Constraints
	PolicyVersion    string
	ExpiresAt        time.Time
	Nonce            string
}

// ValidFor checks every bound field against the request and the current
// policy version at time now. Any mismatch is approval_invalid. Nil
// arguments are programming errors and return invalid_argument.
func (g *ApprovalGrant) ValidFor(request *ToolRequest, policyVersion string, now time.Time) error {
	if g == nil {
		return Errorf(ErrorCodeInvalidArgument, "grant must not be nil")
	}
	if request == nil {
		return Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if g.GrantID == "" || g.Nonce == "" {
		return Errorf(ErrorCodeApprovalInvalid, "grant is missing its identity or nonce")
	}
	if g.PrincipalID != request.PrincipalID {
		return Errorf(ErrorCodeApprovalInvalid, "grant principal %q does not match request principal %q", g.PrincipalID, request.PrincipalID)
	}
	if g.SessionID != request.SessionID {
		return Errorf(ErrorCodeApprovalInvalid, "grant session %q does not match request session %q", g.SessionID, request.SessionID)
	}
	if g.ToolName != request.ToolName {
		return Errorf(ErrorCodeApprovalInvalid, "grant tool %q does not match request tool %q", g.ToolName, request.ToolName)
	}
	if g.ToolVersion != request.ToolVersion {
		return Errorf(ErrorCodeApprovalInvalid, "grant tool version %q does not match request version %q", g.ToolVersion, request.ToolVersion)
	}
	if g.ArgumentsHash != HashArguments(request.Arguments) {
		return Errorf(ErrorCodeApprovalInvalid, "grant arguments hash %q does not match request arguments", g.ArgumentsHash)
	}
	if g.RequestDigest != request.RequestDigest {
		return Errorf(ErrorCodeApprovalInvalid, "grant request digest does not match request")
	}
	if g.CapabilitiesHash != HashCapabilities(request.Capabilities) {
		return Errorf(ErrorCodeApprovalInvalid, "grant capabilities hash %q does not match request capabilities", g.CapabilitiesHash)
	}
	if g.PolicyVersion != policyVersion {
		return Errorf(ErrorCodeApprovalInvalid, "grant policy version %q does not match current %q", g.PolicyVersion, policyVersion)
	}
	if !now.Before(g.ExpiresAt) {
		return Errorf(ErrorCodeApprovalInvalid, "grant expired at %s", g.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

func (g *ApprovalGrant) ValidForConstraints(request *ToolRequest, policyVersion string, now time.Time, expected Constraints) error {
	if err := g.ValidFor(request, policyVersion, now); err != nil {
		return err
	}
	if g.Constraints != expected {
		return Errorf(ErrorCodeApprovalInvalid, "grant constraints do not match current policy")
	}
	return nil
}

// HashArguments returns the canonical hex SHA-256 of the raw arguments, so
// the grant binds the exact bytes evaluated, and the request cannot lie
// about its own hash.
func HashArguments(arguments json.RawMessage) string {
	sum := sha256.Sum256(arguments)
	return hex.EncodeToString(sum[:])
}

// HashCapabilities returns the canonical hex SHA-256 of a capability set.
// Ordering is irrelevant; the set is sorted before hashing, so a grant
// stays valid across request orderings but is invalidated the moment the
// granted capabilities change.
func HashCapabilities(capabilities []string) string {
	sorted := slices.Clone(capabilities)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:])
}
