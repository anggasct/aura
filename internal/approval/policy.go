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

func (t TrustLabel) IsUntrusted() bool {
	return t == TrustUntrustedExternal || t == TrustDerivedUntrusted
}

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
	AllowNetwork   bool          `json:"allow_network"`
	MaxOutputBytes int64         `json:"max_output_bytes"`
	Timeout        time.Duration `json:"timeout"`
}

type PolicyDecision struct {
	Outcome       string      `json:"outcome"` // allow, deny, require_approval
	PolicyVersion string      `json:"policy_version"`
	ReasonCode    string      `json:"reason_code"`
	Constraints   Constraints `json:"constraints"`
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

type ToolBroker interface {
	Evaluate(ctx context.Context, request *ToolRequest) (PolicyDecision, error)
	Execute(ctx context.Context, request *ToolRequest, grant *ApprovalGrant) (ToolResult, error)
}

type Rule struct {
	ToolName             string
	ToolVersion          string
	RequiresApproval     bool
	RequiredCapabilities []string
	AllowedTrust         []TrustLabel
	Constraints          Constraints
}

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

type ApprovalGrant struct {
	GrantID          string      `json:"grant_id"`
	PrincipalID      string      `json:"principal_id"`
	SessionID        string      `json:"session_id"`
	ToolName         string      `json:"tool_name"`
	ToolVersion      string      `json:"tool_version"`
	ArgumentsHash    string      `json:"arguments_hash"`
	RequestDigest    string      `json:"request_digest"`
	CapabilitiesHash string      `json:"capabilities_hash"`
	Constraints      Constraints `json:"constraints"`
	PolicyVersion    string      `json:"policy_version"`
	ExpiresAt        time.Time   `json:"expires_at"`
	Nonce            string      `json:"nonce"`
}

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

func HashArguments(arguments json.RawMessage) string {
	sum := sha256.Sum256(arguments)
	return hex.EncodeToString(sum[:])
}

func HashCapabilities(capabilities []string) string {
	sorted := slices.Clone(capabilities)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:])
}
