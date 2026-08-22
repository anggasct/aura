package toolbroker

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools"
)

type ToolRequest struct {
	RequestID      string
	TurnID         string
	SessionID      string
	PrincipalID    string
	ToolName       string
	ToolVersion    string
	Arguments      json.RawMessage
	RequestDigest  string
	Capabilities   []string
	Trust          approval.TrustLabel
	Deadline       time.Time
	IdempotencyKey string
	Approval       *approval.ApprovalGrant
}

type ToolResult struct {
	ToolName    string
	ToolVersion string
	Class       ResultClass
	Untrusted   bool
	Output      json.RawMessage
	Truncated   bool
}

type Adapter func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error)

type Observation struct {
	ToolName    string
	ToolVersion string
	Class       ResultClass
	Duration    time.Duration
	OutputBytes int64
}

type Observer func(Observation)

type Options struct {
	Policy               approval.Policy
	Adapters             map[string]Adapter
	Secrets              []string
	MaxInlineResultBytes int64
	Observer             Observer
}

type Broker struct {
	definitions          map[string]*tools.Definition
	engine               *approval.Engine
	adapters             map[string]Adapter
	secrets              []string
	maxInlineResultBytes int64
	observer             Observer
}

func New(options Options) (*Broker, error) {
	definitions := tools.DefinitionsByKey()
	policy := options.Policy
	if policy.Version == "" {
		policy = DefaultPolicy()
	}
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("toolbroker: invalid policy: %w", err)
	}
	if options.MaxInlineResultBytes <= 0 {
		options.MaxInlineResultBytes = 64 * 1024
	}
	engine, err := approval.NewEngine(policy, func(context.Context, approval.ToolRequest, approval.Constraints) (approval.ToolResult, error) {
		return approval.ToolResult{}, errors.New("toolbroker: direct approval handler is unavailable")
	})
	if err != nil {
		return nil, err
	}
	return &Broker{
		definitions:          definitions,
		engine:               engine,
		adapters:             cloneAdapters(options.Adapters),
		secrets:              slices.Clone(options.Secrets),
		maxInlineResultBytes: options.MaxInlineResultBytes,
		observer:             options.Observer,
	}, nil
}

func DefaultPolicy() approval.Policy {
	rules := make(map[string]approval.Rule)
	for _, definition := range tools.Builtins() {
		allowedTrust := []approval.TrustLabel{
			approval.TrustOwnerInput,
			approval.TrustTrustedConfiguration,
		}
		rules[definition.Name] = approval.Rule{
			ToolName:             definition.Name,
			ToolVersion:          definition.Version,
			RequiresApproval:     definition.RequiresApproval,
			RequiredCapabilities: slices.Clone(definition.RequiredCapabilities),
			AllowedTrust:         allowedTrust,
			Constraints: approval.Constraints{
				MaxOutputBytes: 64 * 1024,
				Timeout:        30 * time.Second,
			},
		}
	}
	return approval.Policy{Version: "builtin-tools-v1", Rules: rules}
}

func (b *Broker) Definitions() []tools.Definition {
	definitions := make([]tools.Definition, 0, len(b.definitions))
	for _, definition := range b.definitions {
		definitionCopy := *definition
		definitionCopy.Schema = slices.Clone(definition.Schema)
		definitionCopy.RequiredCapabilities = slices.Clone(definition.RequiredCapabilities)
		definitions = append(definitions, definitionCopy)
	}
	slices.SortFunc(definitions, func(a, c tools.Definition) int {
		return cmp.Compare(a.Key(), c.Key())
	})
	return definitions
}

func (b *Broker) PolicyVersion() string {
	return b.engine.PolicyVersion()
}

func (b *Broker) Evaluate(ctx context.Context, request *ToolRequest) (approval.PolicyDecision, error) {
	canonical, err := b.canonicalRequest(request)
	if err != nil {
		return approval.PolicyDecision{}, err
	}
	return b.engine.Evaluate(ctx, toApprovalRequest(&canonical, b.PolicyVersion()))
}

func (b *Broker) Grant(ctx context.Context, request *ToolRequest, ttl time.Duration) (approval.ApprovalGrant, error) {
	canonical, err := b.canonicalRequest(request)
	if err != nil {
		return approval.ApprovalGrant{}, err
	}
	grant, err := b.engine.Grant(ctx, toApprovalRequest(&canonical, b.PolicyVersion()), ttl)
	if err != nil {
		return approval.ApprovalGrant{}, mapApprovalError(err)
	}
	return grant, nil
}

func (b *Broker) Execute(ctx context.Context, request *ToolRequest) (result ToolResult, err error) {
	started := time.Now()
	defer func() {
		if b.observer == nil {
			return
		}
		class := result.Class
		if class == "" {
			class, _ = CodeOf(err)
		}
		if class == "" {
			class = ResultExecutionFailed
		}
		observation := Observation{Class: class, Duration: time.Since(started), OutputBytes: int64(len(result.Output))}
		if request != nil {
			observation.ToolName = request.ToolName
			observation.ToolVersion = request.ToolVersion
		}
		b.observer(observation)
	}()
	canonical, err := b.canonicalRequest(request)
	if err != nil {
		return ToolResult{}, err
	}
	if err := contextError(ctx, canonical.Deadline); err != nil {
		return ToolResult{}, err
	}
	decision, err := b.engine.Evaluate(ctx, toApprovalRequest(&canonical, b.PolicyVersion()))
	if err != nil {
		return ToolResult{}, mapApprovalError(err)
	}
	key := canonical.ToolName + "@" + canonical.ToolVersion
	adapter, ok := b.adapters[key]
	if !ok {
		return ToolResult{}, errorf(ResultCapabilityUnavailable, "tool %q is not available in this artifact", key)
	}

	grant := canonical.Approval
	if decision.Outcome == approval.OutcomeRequireApproval && grant == nil {
		return ToolResult{}, errorf(ResultApprovalRequired, "tool %q requires approval", key)
	}
	if grant == nil {
		ttl := 5 * time.Minute
		if !canonical.Deadline.IsZero() {
			ttl = time.Until(canonical.Deadline)
		}
		newGrant, grantErr := b.engine.Grant(ctx, toApprovalRequest(&canonical, b.PolicyVersion()), ttl)
		err = grantErr
		if err != nil {
			return ToolResult{}, mapApprovalError(err)
		}
		grant = &newGrant
	}
	if err := b.engine.ValidateAndConsume(ctx, toApprovalRequest(&canonical, b.PolicyVersion()), grant); err != nil {
		return ToolResult{}, mapApprovalError(err)
	}
	result, err = adapter(ctx, &canonical, decision.Constraints)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{}, errorf(ResultDeadlineExceeded, "tool request ended: %v", err)
		}
		if code, ok := sandbox.CodeOf(err); ok {
			switch code {
			case sandbox.ErrorCodeSandboxUnavailable:
				return ToolResult{}, errorf(ResultCapabilityUnavailable, "%v", err)
			case sandbox.ErrorCodeSandboxPathDenied, sandbox.ErrorCodeSandboxViolation, sandbox.ErrorCodeSandboxSyscallDenied:
				return ToolResult{}, errorf(ResultPolicyDenied, "%v", err)
			default:
				return ToolResult{}, errorf(ResultExecutionFailed, "%v", err)
			}
		}
		if code, ok := egress.CodeOf(err); ok && code == egress.ErrorCodeEgressDenied {
			return ToolResult{}, errorf(ResultPolicyDenied, "%v", err)
		}
		if contextError(ctx, canonical.Deadline) != nil {
			return ToolResult{}, contextError(ctx, canonical.Deadline)
		}
		return ToolResult{}, errorf(ResultExecutionFailed, "tool %q failed: %s", key, redact([]byte(err.Error()), b.secrets))
	}
	result.ToolName = canonical.ToolName
	result.ToolVersion = canonical.ToolVersion
	result.Untrusted = true
	result.Output = redact(result.Output, b.secrets)
	if result.Class == "" {
		result.Class = ResultOK
	}
	maxOutputBytes := b.maxInlineResultBytes
	if decision.Constraints.MaxOutputBytes > 0 && decision.Constraints.MaxOutputBytes < maxOutputBytes {
		maxOutputBytes = decision.Constraints.MaxOutputBytes
	}
	if int64(len(result.Output)) > maxOutputBytes {
		result.Output = slices.Clone(result.Output[:int(maxOutputBytes)])
		result.Truncated = true
	}
	return result, nil
}

func (b *Broker) canonicalRequest(request *ToolRequest) (ToolRequest, error) {
	if request == nil {
		return ToolRequest{}, errorf(ResultInvalidArgument, "request must not be nil")
	}
	definition, ok := b.definitions[request.ToolName+"@"+request.ToolVersion]
	if !ok {
		return ToolRequest{}, errorf(ResultPolicyDenied, "unknown tool %q version %q", request.ToolName, request.ToolVersion)
	}
	arguments, err := definition.Validate(request.Arguments)
	if err != nil {
		return ToolRequest{}, errorf(ResultInvalidArgument, "%v", err)
	}
	canonical := *request
	canonical.Arguments = arguments
	canonical.Capabilities = slices.Clone(request.Capabilities)
	slices.Sort(canonical.Capabilities)
	canonical.Approval = request.Approval
	canonicalDigest, err := requestDigest(&canonical, b.PolicyVersion())
	if err != nil {
		return ToolRequest{}, errorf(ResultInvalidArgument, "canonicalize request: %v", err)
	}
	canonical.RequestDigest = canonicalDigest
	return canonical, nil
}

func toApprovalRequest(request *ToolRequest, policyVersion string) *approval.ToolRequest {
	digest := request.RequestDigest
	if digest == "" {
		digest, _ = requestDigest(request, policyVersion)
	}
	return &approval.ToolRequest{
		RequestID:      request.RequestID,
		TurnID:         request.TurnID,
		SessionID:      request.SessionID,
		PrincipalID:    request.PrincipalID,
		ToolName:       request.ToolName,
		ToolVersion:    request.ToolVersion,
		Arguments:      request.Arguments,
		RequestDigest:  digest,
		Capabilities:   slices.Clone(request.Capabilities),
		Trust:          request.Trust,
		Deadline:       request.Deadline,
		IdempotencyKey: request.IdempotencyKey,
	}
}

func requestDigest(request *ToolRequest, policyVersion string) (string, error) {
	payload := struct {
		RequestID      string          `json:"request_id"`
		TurnID         string          `json:"turn_id"`
		SessionID      string          `json:"session_id"`
		PrincipalID    string          `json:"principal_id"`
		ToolName       string          `json:"tool_name"`
		ToolVersion    string          `json:"tool_version"`
		Arguments      json.RawMessage `json:"arguments"`
		Capabilities   []string        `json:"capabilities"`
		Trust          string          `json:"trust"`
		IdempotencyKey string          `json:"idempotency_key"`
		PolicyVersion  string          `json:"policy_version"`
	}{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, Capabilities: slices.Clone(request.Capabilities), Trust: string(request.Trust),
		IdempotencyKey: request.IdempotencyKey, PolicyVersion: policyVersion,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func contextError(ctx context.Context, deadline time.Time) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return errorf(ResultDeadlineExceeded, "tool request ended: %v", err)
		}
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return errorf(ResultDeadlineExceeded, "tool request deadline exceeded")
	}
	return nil
}

func mapApprovalError(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := approval.CodeOf(err); ok {
		switch code {
		case approval.ErrorCodeInvalidArgument:
			return errorf(ResultInvalidArgument, "%v", err)
		case approval.ErrorCodeCapabilityUnavailable:
			return errorf(ResultCapabilityUnavailable, "%v", err)
		case approval.ErrorCodeApprovalRequired:
			return errorf(ResultApprovalRequired, "%v", err)
		case approval.ErrorCodePolicyDenied, approval.ErrorCodeApprovalInvalid:
			return errorf(ResultPolicyDenied, "%v", err)
		}
	}
	return err
}

func redact(data []byte, secrets []string) []byte {
	redacted := slices.Clone(data)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		redacted = []byte(strings.ReplaceAll(string(redacted), secret, "[REDACTED]"))
	}
	return redacted
}

func cloneAdapters(adapters map[string]Adapter) map[string]Adapter {
	if len(adapters) == 0 {
		return map[string]Adapter{}
	}
	return mapsClone(adapters)
}

func mapsClone(adapters map[string]Adapter) map[string]Adapter {
	cloned := make(map[string]Adapter, len(adapters))
	for key, adapter := range adapters {
		cloned[key] = adapter
	}
	return cloned
}
