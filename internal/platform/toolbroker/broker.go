package toolbroker

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/store"
)

type ToolRequest struct {
	RequestID       string
	TurnID          string
	SessionID       string
	PrincipalID     string
	PrincipalClass  principal.Class
	AgentID         string
	ToolName        string
	ToolVersion     string
	Arguments       json.RawMessage
	RequestDigest   string
	Capabilities    []string
	Trust           approval.TrustLabel
	Deadline        time.Time
	IdempotencyKey  string
	Approval        *approval.ApprovalGrant
	EventSequence   uint64
	EventInvocation string
	EventBranch     string
	EventAuthor     string
	Knowledge       []string
	Tainted         bool

	AllocateEventSequence bool
}

type ToolResult struct {
	ToolName    string          `json:"tool_name"`
	ToolVersion string          `json:"tool_version"`
	Class       ResultClass     `json:"class"`
	Untrusted   bool            `json:"untrusted"`
	Output      json.RawMessage `json:"output"`
	Truncated   bool            `json:"truncated"`
	Audit       json.RawMessage `json:"-"`
}

type Adapter func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error)

const (
	PolicyOutcomeAllow           = "allow"
	PolicyOutcomeRequireApproval = "require_approval"
	PolicyOutcomeDeny            = "deny"
	PolicyOutcomeNotEvaluated    = "not_evaluated"

	ApprovalMissing  = "missing"
	ApprovalAttached = "attached"
	ApprovalAuto     = "auto"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalStanding = "standing"
	ApprovalReplayed = "replayed"

	ExecutorDirect = "direct"
	ExecutorEffect = "effect"
)

type Observation struct {
	ToolName      string
	ToolVersion   string
	Class         ResultClass
	PolicyOutcome string
	Approval      string
	Executor      string
	Duration      time.Duration
	OutputBytes   int64
}

type Observer func(context.Context, Observation)

type ApprovalPrompt struct {
	ToolName       string
	ToolVersion    string
	SessionID      string
	TurnID         string
	PrincipalID    string
	Arguments      string
	Network        bool
	Timeout        time.Duration
	MaxOutputBytes int64
	PolicyVersion  string
	ReasonCode     string
	ExpiresAt      time.Time

	StandingGrantAllowed bool
}

type ApprovalDecider func(ctx context.Context, prompt *ApprovalPrompt) (bool, error)

type AccessPolicy interface {
	ToolCeiling(agentID string, class principal.Class) (tools []string, known bool)
	StandingGrant(ctx context.Context, principalID, agentID, toolName, server string) (bool, error)
}

type ApprovalGuard func(ctx context.Context, request *ToolRequest) (required bool, err error)

type Options struct {
	Policy               approval.Policy
	Adapters             map[string]Adapter
	Secrets              []string
	MaxInlineResultBytes int64
	Artifacts            store.ArtifactStore
	Effects              *effect.Executor
	Observer             Observer
	ApprovalDecider      ApprovalDecider
	Access               AccessPolicy
	ApprovalGuards       map[string]ApprovalGuard
	ApprovalReachable    func(sessionID string) bool
	HostCapabilities     []string
	ArgumentChecks       map[string]ArgumentCheck
}

type ArgumentCheck func(arguments json.RawMessage) error

type Broker struct {
	mu                   sync.RWMutex
	definitions          map[string]*tools.Definition
	engine               *approval.Engine
	adapters             map[string]Adapter
	secretMu             sync.RWMutex
	secrets              []string
	maxInlineResultBytes int64
	artifacts            store.ArtifactStore
	effects              *effect.Executor
	observer             Observer
	decider              ApprovalDecider
	access               AccessPolicy
	guards               map[string]ApprovalGuard
	approvalReachable    func(sessionID string) bool
	argumentChecks       map[string]ArgumentCheck
}

func New(options *Options) (*Broker, error) {
	if options == nil {
		options = &Options{}
	}
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
	}, options.HostCapabilities)
	if err != nil {
		return nil, err
	}
	return &Broker{
		definitions:          definitions,
		engine:               engine,
		adapters:             cloneAdapters(options.Adapters),
		secrets:              slices.Clone(options.Secrets),
		maxInlineResultBytes: options.MaxInlineResultBytes,
		artifacts:            options.Artifacts,
		effects:              options.Effects,
		observer:             options.Observer,
		decider:              options.ApprovalDecider,
		access:               options.Access,
		guards:               maps.Clone(options.ApprovalGuards),
		approvalReachable:    options.ApprovalReachable,
		argumentChecks:       maps.Clone(options.ArgumentChecks),
	}, nil
}

func DefaultPolicy() approval.Policy {
	rules := make(map[string]approval.Rule)
	listed := tools.Builtins()
	for i := range listed {
		definition := &listed[i]
		allowedTrust := []approval.TrustLabel{
			approval.TrustOwnerInput,
			approval.TrustTrustedConfiguration,
			approval.TrustDerivedUntrusted,
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

func (b *Broker) RegisterSecret(value string) {
	if value == "" {
		return
	}
	b.secretMu.Lock()
	defer b.secretMu.Unlock()
	if !slices.Contains(b.secrets, value) {
		b.secrets = append(b.secrets, value)
	}
}

func (b *Broker) secretValues() []string {
	b.secretMu.RLock()
	defer b.secretMu.RUnlock()
	return slices.Clone(b.secrets)
}

func (b *Broker) Definitions() []tools.Definition {
	b.mu.RLock()
	definitions := make([]tools.Definition, 0, len(b.definitions))
	for _, definition := range b.definitions {
		definitionCopy := *definition
		definitionCopy.Schema = slices.Clone(definition.Schema)
		definitionCopy.RequiredCapabilities = slices.Clone(definition.RequiredCapabilities)
		definitions = append(definitions, definitionCopy)
	}
	b.mu.RUnlock()
	slices.SortFunc(definitions, func(a, c tools.Definition) int {
		return cmp.Compare(a.Key(), c.Key())
	})
	return definitions
}

func (b *Broker) RegisterTool(definition *tools.Definition, adapter Adapter, rule *approval.Rule) error {
	if definition == nil {
		return errorf(ResultInvalidArgument, "tool definition must not be nil")
	}
	if adapter == nil {
		return errorf(ResultInvalidArgument, "adapter must not be nil")
	}
	if rule == nil {
		return errorf(ResultInvalidArgument, "rule must not be nil")
	}
	if definition.Name == "" || definition.Version == "" {
		return errorf(ResultInvalidArgument, "tool definition requires name and version")
	}
	if rule.ToolName != definition.Name || rule.ToolVersion != definition.Version {
		return errorf(ResultInvalidArgument, "rule tool %q version %q does not match definition %q version %q", rule.ToolName, rule.ToolVersion, definition.Name, definition.Version)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	defCopy := *definition
	defCopy.Schema = slices.Clone(definition.Schema)
	defCopy.RequiredCapabilities = slices.Clone(definition.RequiredCapabilities)

	key := defCopy.Key()
	oldDef, hadOldDef := b.definitions[key]
	oldAdapter, hadOldAdapter := b.adapters[key]

	b.definitions[key] = &defCopy
	b.adapters[key] = adapter

	if err := b.engine.RegisterRule(rule); err != nil {
		if hadOldDef {
			b.definitions[key] = oldDef
		} else {
			delete(b.definitions, key)
		}
		if hadOldAdapter {
			b.adapters[key] = oldAdapter
		} else {
			delete(b.adapters, key)
		}
		return err
	}
	return nil
}

func (b *Broker) UnregisterTool(toolName, toolVersion string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := toolName + "@" + toolVersion
	delete(b.definitions, key)
	delete(b.adapters, key)
	b.engine.UnregisterRule(toolName)
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
	var policyOutcome, approvalState, executorClass string
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
		if policyOutcome == "" {
			policyOutcome = PolicyOutcomeNotEvaluated
		}
		observation := Observation{Class: class, PolicyOutcome: policyOutcome, Approval: approvalState, Executor: executorClass, Duration: time.Since(started), OutputBytes: int64(len(result.Output))}
		if request != nil {
			observation.ToolName = request.ToolName
			observation.ToolVersion = request.ToolVersion
		}
		b.observer(ctx, observation)
	}()
	canonical, err := b.canonicalRequest(request)
	if err != nil {
		return ToolResult{}, err
	}
	if err := contextError(ctx, canonical.Deadline); err != nil {
		return ToolResult{}, err
	}
	decision, err := b.evaluatePolicy(ctx, toApprovalRequest(&canonical, b.PolicyVersion()))
	if err != nil {
		policyOutcome = PolicyOutcomeDeny
		return ToolResult{}, mapApprovalError(err)
	}
	policyOutcome = decision.Outcome
	key := canonical.ToolName + "@" + canonical.ToolVersion
	b.mu.RLock()
	definition := b.definitions[key]
	adapter, ok := b.adapters[key]
	b.mu.RUnlock()
	if !ok {
		return ToolResult{}, errorf(ResultCapabilityUnavailable, "tool %q is not available in this artifact", key)
	}
	class := requestClass(&canonical)
	if err := b.enforceToolTier(&canonical, class, definition); err != nil {
		policyOutcome = PolicyOutcomeDeny
		return ToolResult{}, err
	}
	guarded, err := b.guardRequires(ctx, &canonical)
	if err != nil {
		policyOutcome = PolicyOutcomeDeny
		return ToolResult{}, err
	}
	settled := false
	tainted := canonical.Tainted && definition.IsExternalSink()
	if (guarded || tainted || (class != principal.ClassOwner && definition.Effectful)) && decision.Outcome == approval.OutcomeAllow {
		decision.Outcome = approval.OutcomeRequireApproval
		policyOutcome = decision.Outcome
	}
	if canonical.Tainted && decision.Outcome == approval.OutcomeRequireApproval {
		decision.ReasonCode = approval.ReasonUntrustedInput
	}
	if definition.Effectful {
		if b.effects == nil {
			return ToolResult{}, errorf(ResultCapabilityUnavailable, "effect journal is required for tool %q", key)
		}
		_, scoped := durable.TurnScopeFrom(ctx)
		if err := validateEffectRequest(&canonical, scoped); err != nil {
			return ToolResult{}, err
		}
		if !scoped {
			settled, err = b.settledReplay(ctx, &canonical)
			if err != nil {
				return ToolResult{}, err
			}
		}
	}

	switch {
	case settled:
		approvalState = ApprovalReplayed
	case canonical.Approval != nil:
		approvalState = ApprovalAttached
		scope, _ := durable.TurnScopeFrom(ctx)
		if err := b.validateDurable(ctx, scope, &canonical, canonical.Approval); err != nil {
			return ToolResult{}, mapApprovalError(err)
		}
	default:
		standing := false
		standingAllowed := definition.StandingGrantAllowed() && !guarded
		if decision.Outcome == approval.OutcomeRequireApproval && class == principal.ClassTrusted && standingAllowed && !canonical.Tainted {
			standing, err = b.standingGrant(ctx, &canonical, definition.Server)
			if err != nil {
				approvalState = ApprovalRejected
				return ToolResult{}, err
			}
		}
		approvalState, err = b.authorize(ctx, &authorization{canonical: &canonical, definition: definition, decision: decision, expiry: absoluteApprovalExpiry(canonical.Deadline), standing: standing, standingAllowed: standingAllowed})
		if err != nil {
			if code, _ := CodeOf(err); code == ResultUntrustedInputNoApprover {
				policyOutcome = PolicyOutcomeDeny
			}
			return ToolResult{}, err
		}
	}
	if scope, ok := durable.TurnScopeFrom(ctx); ok {
		result, err = b.executeDurable(ctx, scope, &canonical, decision.Constraints, adapter, definition.Effectful)
	} else {
		if definition.Effectful {
			result, err = b.executeEffect(ctx, &canonical, decision.Constraints, adapter)
		} else {
			result, err = adapter(ctx, &canonical, decision.Constraints)
		}
		if err != nil {
			err = b.classifyExecutionError(ctx, &canonical, err)
		}
	}
	if definition.Effectful {
		executorClass = ExecutorEffect
	} else {
		executorClass = ExecutorDirect
	}
	if err != nil {
		return ToolResult{}, err
	}
	return b.finalize(ctx, &canonical, &pendingResult{result: result, limit: b.outputLimit(decision.Constraints)})
}

type pendingResult struct {
	result   ToolResult
	limit    int64
	recorded bool
}

func (b *Broker) outputLimit(constraints approval.Constraints) int64 {
	limit := b.maxInlineResultBytes
	if constraints.MaxOutputBytes > 0 && constraints.MaxOutputBytes < limit {
		limit = constraints.MaxOutputBytes
	}
	return limit
}

func (b *Broker) finalize(ctx context.Context, canonical *ToolRequest, pending *pendingResult) (ToolResult, error) {
	if canonical == nil {
		return ToolResult{}, errorf(ResultInvalidArgument, "tool request must not be nil")
	}
	if pending == nil {
		return ToolResult{}, errorf(ResultInvalidArgument, "pending result must not be nil")
	}
	result := pending.result
	result.ToolName = canonical.ToolName
	result.ToolVersion = canonical.ToolVersion
	result.Untrusted = true
	result.Output = redactOutput(result.Output, b.secretValues())
	if result.Class == "" {
		result.Class = ResultOK
	}
	maxOutputBytes := pending.limit
	if pending.recorded {
		recordLimit, err := recordOutputLimit(&result)
		if err != nil {
			return ToolResult{}, err
		}
		maxOutputBytes = min(maxOutputBytes, recordLimit)
	}
	if int64(len(result.Output)) > maxOutputBytes {
		bounded, err := b.boundOutput(ctx, canonical, result.Output, maxOutputBytes)
		if err != nil {
			return ToolResult{}, err
		}
		result.Output = bounded
		result.Truncated = true
	}
	return result, nil
}

type boundedOutput struct {
	ArtifactID string `json:"artifact_id,omitempty"`
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"size_bytes"`
	Truncated  bool   `json:"truncated"`
	Body       string `json:"body,omitempty"`
}

func (b *Broker) boundOutput(ctx context.Context, canonical *ToolRequest, output json.RawMessage, maxOutputBytes int64) (json.RawMessage, error) {
	digest := sha256.Sum256(output)
	envelope := boundedOutput{
		Digest:    hex.EncodeToString(digest[:]),
		SizeBytes: int64(len(output)),
		Truncated: true,
	}
	if b.artifacts != nil && canonical.SessionID != "" {
		artifactID, err := randomArtifactID()
		if err != nil {
			return nil, errorf(ResultExecutionFailed, "generate artifact id: %v", err)
		}
		ref, err := b.artifacts.Put(ctx, strings.NewReader(string(output)), &store.ArtifactMetadata{
			ID:        artifactID,
			SessionID: canonical.SessionID,
			Filename:  canonical.ToolName + "-" + canonical.RequestID + ".json",
			MediaType: "application/json",
		})
		if err == nil {
			envelope.ArtifactID = ref.ID
			envelope.Digest = ref.BlobDigest
			envelope.Truncated = false
		}
	}
	if envelope.ArtifactID == "" {
		bodyBytes, err := boundedBodyBytes(envelope, output, maxOutputBytes)
		if err != nil {
			return nil, err
		}
		envelope.Body = string(output[:bodyBytes])
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, errorf(ResultExecutionFailed, "encode bounded result: %v", err)
	}
	if int64(len(encoded)) > maxOutputBytes {
		return nil, errorf(ResultExecutionFailed, "bounded result envelope exceeds output limit")
	}
	return encoded, nil
}

func boundedBodyBytes(envelope boundedOutput, output json.RawMessage, maxOutputBytes int64) (int, error) {
	base := envelope
	base.Body = ""
	encoded, err := json.Marshal(base)
	if err != nil {
		return 0, errorf(ResultExecutionFailed, "encode bounded result: %v", err)
	}
	if int64(len(encoded)) > maxOutputBytes {
		return 0, errorf(ResultExecutionFailed, "bounded result envelope exceeds output limit")
	}
	low, high := 0, len(output)
	for low < high {
		mid := low + (high-low+1)/2
		candidate := envelope
		candidate.Body = string(output[:mid])
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return 0, errorf(ResultExecutionFailed, "encode bounded result: %v", err)
		}
		if int64(len(encoded)) <= maxOutputBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return low, nil
}

type effectInvocation struct {
	adapter     Adapter
	request     *ToolRequest
	constraints approval.Constraints
	limit       int64
	finalize    func(context.Context, *pendingResult) (ToolResult, error)
	result      ToolResult
	invoked     bool
	notDone     error
	failure     error
}

func (i *effectInvocation) SupportsIdempotency() bool { return false }

func (i *effectInvocation) Invoke(ctx context.Context, _ *effect.Invocation) (effect.Outcome, error) {
	result, err := i.adapter(ctx, i.request, i.constraints)
	if err != nil {
		var notPerformed *NotPerformedError
		if errors.As(err, &notPerformed) {
			i.notDone = err
			return effect.Outcome{SafeErrorCode: notPerformed.Code}, nil
		}
		return effect.Outcome{}, err
	}
	i.result = result
	i.invoked = true
	digest := sha256.Sum256(result.Output)
	audit := result.Audit
	if !json.Valid(audit) {
		audit = nil
	}
	receipt, err := json.Marshal(struct {
		ToolName     string          `json:"tool_name"`
		ToolVersion  string          `json:"tool_version"`
		Class        ResultClass     `json:"class"`
		OutputBytes  int64           `json:"output_bytes"`
		OutputDigest string          `json:"output_digest"`
		Truncated    bool            `json:"truncated"`
		Audit        json.RawMessage `json:"audit,omitempty"`
	}{
		ToolName: i.request.ToolName, ToolVersion: i.request.ToolVersion,
		Class: result.Class, OutputBytes: int64(len(result.Output)),
		OutputDigest: hex.EncodeToString(digest[:]), Truncated: result.Truncated, Audit: audit,
	})
	if err != nil {
		return effect.Outcome{}, err
	}
	return i.record(ctx, receipt, &result), nil
}

func (i *effectInvocation) record(ctx context.Context, receipt json.RawMessage, result *ToolResult) effect.Outcome {
	unrecorded := effect.Outcome{Succeeded: true, Receipt: receipt, UnrecordedResult: true}
	finalized, err := i.finalize(ctx, &pendingResult{result: *result, limit: i.limit, recorded: true})
	if err != nil {
		i.failure = err
		return unrecorded
	}
	i.result = finalized
	record, err := encodeRecord(&finalized)
	if err != nil {
		return unrecorded
	}
	decoded, err := decodeRecord(record)
	if err != nil {
		return unrecorded
	}
	i.result = decoded
	return effect.Outcome{Succeeded: true, Receipt: receipt, Result: record}
}

const effectProvider = "builtin-tools"

func effectPayload(request *ToolRequest) (json.RawMessage, error) {
	payload, err := json.Marshal(struct {
		ToolName    string          `json:"tool_name"`
		ToolVersion string          `json:"tool_version"`
		Arguments   json.RawMessage `json:"arguments"`
	}{request.ToolName, request.ToolVersion, request.Arguments})
	if err != nil {
		return nil, errorf(ResultInvalidArgument, "encode effect request: %v", err)
	}
	return payload, nil
}

func (b *Broker) settledReplay(ctx context.Context, request *ToolRequest) (bool, error) {
	payload, err := effectPayload(request)
	if err != nil {
		return false, err
	}
	intent, found, err := b.effects.Journal().Lookup(ctx, effectProvider, request.ToolName, request.IdempotencyKey, payload)
	if err != nil {
		if code, ok := effect.CodeOf(err); ok && code == effect.ErrorCodeIdempotencyConflict {
			return false, errorf(ResultInvalidArgument, "tool %q was called again under its idempotency key with a different request", request.ToolName)
		}
		return false, err
	}
	if !found {
		return false, nil
	}
	if intent.SessionID != request.SessionID {
		return false, errorf(ResultInvalidArgument, "tool %q idempotency key belongs to another session", request.ToolName)
	}
	return intent.State != effect.StatePrepared, nil
}

func (b *Broker) executeEffect(ctx context.Context, request *ToolRequest, constraints approval.Constraints, adapter Adapter) (ToolResult, error) {
	payload, err := effectPayload(request)
	if err != nil {
		return ToolResult{}, err
	}
	invocation := &effectInvocation{
		adapter: adapter, request: request, constraints: constraints, limit: b.outputLimit(constraints),
		finalize: func(ctx context.Context, pending *pendingResult) (ToolResult, error) {
			return b.finalize(ctx, request, pending)
		},
	}
	intent, err := b.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID: request.SessionID, TurnID: request.TurnID, ToolCallID: request.RequestID,
		IdempotencyKey: request.IdempotencyKey, Provider: effectProvider, Operation: request.ToolName,
		Classification: effect.ClassificationEffectful, Request: payload,
		EventSequence: request.EventSequence, AllocateSequence: request.AllocateEventSequence, EventInvocation: request.EventInvocation,
		EventBranch: request.EventBranch, EventAuthor: request.EventAuthor,
	}, invocation)
	if err != nil {
		if code, ok := effect.CodeOf(err); ok && code == effect.ErrorCodeIdempotencyConflict {
			return ToolResult{}, errorf(ResultInvalidArgument, "tool %q was called again under its idempotency key with a different request", request.ToolName)
		}
		return ToolResult{}, err
	}
	if invocation.notDone != nil && intent.State == effect.StateFailed {
		return ToolResult{}, invocation.notDone
	}
	if intent.State != effect.StateSucceeded {
		return ToolResult{}, errorf(ResultExecutionFailed, "effectful tool settled as %s", intent.State)
	}
	if !invocation.invoked {
		return b.replayRecorded(ctx, request, intent)
	}
	if invocation.failure != nil {
		return ToolResult{}, invocation.failure
	}
	return invocation.result, nil
}

func validateEffectRequest(request *ToolRequest, scoped bool) error {
	if request.AllocateEventSequence && (request.EventSequence != 0 || scoped) {
		return errorf(ResultInvalidArgument, "effectful tool requests that let the journal allocate the event sequence must carry none and run outside a durable turn")
	}
	sequenced := request.EventSequence != 0 || request.AllocateEventSequence
	if request.SessionID == "" || request.TurnID == "" || request.RequestID == "" || request.IdempotencyKey == "" || !sequenced {
		return errorf(ResultInvalidArgument, "effectful tool requests require session, turn, request, idempotency, and event sequence identifiers")
	}
	return nil
}

func randomArtifactID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "art-" + hex.EncodeToString(data[:]), nil
}

const (
	defaultApprovalTTL = 5 * time.Minute
)

func requestClass(request *ToolRequest) principal.Class {
	if request.PrincipalClass == "" {
		return principal.ClassOwner
	}
	return principal.ParseClass(string(request.PrincipalClass))
}

func (b *Broker) enforceToolTier(request *ToolRequest, class principal.Class, definition *tools.Definition) error {
	if class == principal.ClassOwner {
		return nil
	}
	if class == principal.ClassPublic && (definition.Server != "" || definition.Plugin != nil) {
		return errorf(ResultToolTierDenied, "tool %q is not available to public principals", request.ToolName)
	}
	if class == principal.ClassPublic && definition.Effectful {
		return errorf(ResultToolTierDenied, "tool %q is not available to public principals", request.ToolName)
	}
	if b.access == nil {
		return errorf(ResultToolTierDenied, "tool %q is not available to %s principals", request.ToolName, class)
	}
	ceiling, known := b.access.ToolCeiling(request.AgentID, class)
	if !known || !listsInCeiling(ceiling, request.ToolName, definition) {
		return errorf(ResultToolTierDenied, "tool %q is not available to %s principals", request.ToolName, class)
	}
	return nil
}

func listsInCeiling(ceiling []string, name string, definition *tools.Definition) bool {
	if definition.Plugin != nil {
		return tools.ListsPluginTool(ceiling, name, definition.Plugin.ID)
	}
	return tools.ListsTool(ceiling, name, definition.Server)
}

func (b *Broker) sessionReachable(sessionID string) bool {
	return b.approvalReachable != nil && b.approvalReachable(sessionID)
}

func (b *Broker) guardRequires(ctx context.Context, request *ToolRequest) (bool, error) {
	guard := b.guards[request.ToolName]
	if guard == nil {
		return false, nil
	}
	required, err := guard(ctx, request)
	if err != nil {
		return false, errorf(ResultPolicyDenied, "tool %q approval guard failed closed", request.ToolName)
	}
	return required, nil
}

func (b *Broker) standingGrant(ctx context.Context, request *ToolRequest, server string) (bool, error) {
	if b.access == nil || request.AgentID == "" {
		return false, nil
	}
	lookup := func(ctx context.Context) ([]byte, error) {
		active, err := b.access.StandingGrant(ctx, request.PrincipalID, request.AgentID, request.ToolName, server)
		if err != nil {
			return nil, err
		}
		return json.Marshal(active)
	}
	var raw []byte
	var err error
	if scope, ok := durable.TurnScopeFrom(ctx); ok {
		raw, err = scope.Invocation().RunAction(ctx, scope.NextOp(), lookup)
	} else {
		raw, err = lookup(ctx)
	}
	if err != nil {
		return false, errorf(ResultPolicyDenied, "tool %q standing grant lookup failed closed", request.ToolName)
	}
	var active bool
	if err := json.Unmarshal(raw, &active); err != nil {
		return false, errorf(ResultPolicyDenied, "tool %q standing grant lookup failed closed", request.ToolName)
	}
	return active, nil
}

func absoluteApprovalExpiry(deadline time.Time) time.Time {
	if !deadline.IsZero() {
		return deadline
	}
	return time.Now().Add(defaultApprovalTTL)
}

func (b *Broker) approvalText(definition *tools.Definition, arguments json.RawMessage) (string, error) {
	return b.renderedText(arguments, definition.Name, definition.ApprovalText)
}

func (b *Broker) renderedText(arguments json.RawMessage, toolName string, render func(json.RawMessage, func(string) string) (string, error)) (string, error) {
	secrets := b.secretValues()
	display, err := render(arguments, func(text string) string {
		return string(redact([]byte(text), secrets))
	})
	if err != nil {
		code, _ := tools.CodeOf(err)
		if code == tools.ErrorCodeDisplayTooLarge {
			return "", errorf(ResultApprovalDisplayTooLarge, "tool %q arguments do not fit the approval display", toolName)
		}
		if code == tools.ErrorCodeDisplayRedacted {
			return "", errorf(ResultPolicyDenied, "tool %q arguments hold a configured secret, so the approval card cannot show them", toolName)
		}
		return "", errorf(ResultPolicyDenied, "tool %q approval display failed closed", toolName)
	}
	return display, nil
}

func (b *Broker) ExactCallText(toolName string, args json.RawMessage) (string, error) {
	b.mu.RLock()
	definition, ok := b.definitions[toolName+"@"+tools.Version]
	b.mu.RUnlock()
	if !ok {
		return "", errorf(ResultPolicyDenied, "unknown tool %q version %q", toolName, tools.Version)
	}
	arguments, err := b.checkedArguments(definition, args)
	if err != nil {
		return "", err
	}
	return b.renderedText(arguments, definition.Name, definition.ExactApprovalText)
}

func (b *Broker) buildApprovalPrompt(call *authorization) (*ApprovalPrompt, error) {
	request, decision := call.canonical, call.decision
	display, err := b.approvalText(call.definition, request.Arguments)
	if err != nil {
		return nil, err
	}
	return &ApprovalPrompt{
		ToolName:       request.ToolName,
		ToolVersion:    request.ToolVersion,
		SessionID:      request.SessionID,
		TurnID:         request.TurnID,
		PrincipalID:    request.PrincipalID,
		Arguments:      display,
		Network:        decision.Constraints.AllowNetwork,
		Timeout:        decision.Constraints.Timeout,
		MaxOutputBytes: decision.Constraints.MaxOutputBytes,
		PolicyVersion:  decision.PolicyVersion,
		ReasonCode:     decision.ReasonCode,
		ExpiresAt:      call.expiry,

		StandingGrantAllowed: call.standingAllowed,
	}, nil
}

func (b *Broker) checkedArguments(definition *tools.Definition, raw json.RawMessage) (json.RawMessage, error) {
	arguments, err := definition.Validate(raw)
	if err != nil {
		return nil, errorf(ResultInvalidArgument, "%v", err)
	}
	if check := b.argumentChecks[definition.Key()]; check != nil {
		if err := check(arguments); err != nil {
			return nil, errorf(ResultInvalidArgument, "%v", err)
		}
	}
	return arguments, nil
}

func (b *Broker) canonicalRequest(request *ToolRequest) (ToolRequest, error) {
	if request == nil {
		return ToolRequest{}, errorf(ResultInvalidArgument, "request must not be nil")
	}
	b.mu.RLock()
	definition, ok := b.definitions[request.ToolName+"@"+request.ToolVersion]
	b.mu.RUnlock()
	if !ok {
		return ToolRequest{}, errorf(ResultPolicyDenied, "unknown tool %q version %q", request.ToolName, request.ToolVersion)
	}
	arguments, err := b.checkedArguments(definition, request.Arguments)
	if err != nil {
		return ToolRequest{}, err
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

func toolKey(request *ToolRequest) string {
	return request.ToolName + "@" + request.ToolVersion
}

func (b *Broker) classifyExecutionError(ctx context.Context, canonical *ToolRequest, err error) *Error {
	secrets := b.secretValues()
	classified := func(class ResultClass, detail string) *Error {
		return &Error{Class: class, Detail: string(redact([]byte(detail), secrets))}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return classified(ResultDeadlineExceeded, fmt.Sprintf("tool request ended: %v", err))
	}
	if code, ok := CodeOf(err); ok && code.stable() {
		return classified(code, err.Error())
	}
	if code, ok := sandbox.CodeOf(err); ok {
		switch code {
		case sandbox.ErrorCodeSandboxUnavailable:
			return classified(ResultCapabilityUnavailable, err.Error())
		case sandbox.ErrorCodeSandboxPathDenied, sandbox.ErrorCodeSandboxViolation, sandbox.ErrorCodeSandboxSyscallDenied:
			return classified(ResultPolicyDenied, err.Error())
		default:
			return classified(ResultExecutionFailed, err.Error())
		}
	}
	if code, ok := egress.CodeOf(err); ok && code == egress.ErrorCodeEgressDenied {
		return classified(ResultPolicyDenied, err.Error())
	}
	var ended *Error
	if errors.As(contextError(ctx, canonical.Deadline), &ended) {
		return classified(ended.Class, ended.Detail)
	}
	return classified(ResultExecutionFailed, fmt.Sprintf("tool %q failed: %s", toolKey(canonical), err.Error()))
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
