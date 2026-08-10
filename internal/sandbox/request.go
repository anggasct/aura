package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/approval"
)

// SandboxRequest is the elevated execution contract. An elevated run reaches
// the contained executor only when its ApprovalGrantID resolves to a grant
// that still binds every field below, so a request altered after approval can
// never execute. PrincipalID, SessionID, and ToolName bind the approver;
// Executable, Arguments, the path roots, the environment, and the Limits form
// the binding digest.
type SandboxRequest struct {
	RequestID       string
	PrincipalID     string
	SessionID       string
	ToolName        string
	Executable      string
	Arguments       []string
	WorkingDir      string
	ReadOnlyPaths   []string
	ReadWritePaths  []string
	Environment     map[string]string
	Capabilities    []string
	Deadline        time.Time
	Limits          Limits
	ApprovalGrantID string
}

// boundContract is the canonical form hashed into the approval grant. Path
// roots and the environment are sorted so byte-identical requests share a
// digest regardless of map or slice order; Arguments stay ordered because
// argv order is semantically significant.
type boundContract struct {
	Executable     string   `json:"executable"`
	Arguments      []string `json:"arguments"`
	WorkingDir     string   `json:"working_dir"`
	ReadOnlyPaths  []string `json:"read_only_paths"`
	ReadWritePaths []string `json:"read_write_paths"`
	Environment    []string `json:"environment"`
	Limits         Limits   `json:"limits"`
}

// DigestPayload returns the canonical JSON a broker hashes when minting an
// approval grant for req, so the broker and the registry cannot drift on what
// the grant binds.
func DigestPayload(req *SandboxRequest) (json.RawMessage, error) {
	if req == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	readOnly := sortedClone(req.ReadOnlyPaths)
	readWrite := sortedClone(req.ReadWritePaths)
	environment := make([]string, 0, len(req.Environment))
	for key, value := range req.Environment {
		environment = append(environment, key+"="+value)
	}
	environment = sortedClone(environment)
	payload, err := json.Marshal(boundContract{
		Executable:     req.Executable,
		Arguments:      req.Arguments,
		WorkingDir:     req.WorkingDir,
		ReadOnlyPaths:  readOnly,
		ReadWritePaths: readWrite,
		Environment:    environment,
		Limits:         req.Limits,
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func sortedClone(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	clone := slices.Clone(values)
	slices.Sort(clone)
	return clone
}

// registeredGrant holds a grant the registry accepted after confirming it
// binds the request the broker presented.
type registeredGrant struct {
	grant approval.ApprovalGrant
}

// Registry is the in-memory authority that resolves an ApprovalGrantID to a
// bound grant and consumes its one-shot nonce on execution. The broker mints
// grants through the approval engine and registers them here; the contained
// executor resolves them by ID, so the sandbox never trusts a caller-supplied
// grant object directly.
type Registry struct {
	mu             sync.Mutex
	policyVersion  string
	now            func() time.Time
	logger         *slog.Logger
	grants         map[string]registeredGrant
	consumedNonces map[string]struct{}
}

// NewRegistry builds a registry bound to one policy version. The version is
// fixed for the registry's life so a grant minted under a prior policy fails
// validation the moment the broker reloads policy and rebuilds the registry.
func NewRegistry(policyVersion string, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Registry{
		policyVersion:  policyVersion,
		now:            time.Now,
		logger:         logger,
		grants:         map[string]registeredGrant{},
		consumedNonces: map[string]struct{}{},
	}
}

// Register records a grant for req after confirming the grant binds this exact
// request. A grant already registered, or one whose bound fields do not match
// req, is refused so a mismatched or replayed grant can never reach execution.
func (r *Registry) Register(_ context.Context, grant *approval.ApprovalGrant, req *SandboxRequest) error {
	if grant == nil {
		return Errorf(ErrorCodeInvalidArgument, "grant must not be nil")
	}
	if req == nil {
		return Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if grant.GrantID == "" || grant.Nonce == "" {
		return Errorf(ErrorCodeApprovalInvalid, "grant is missing its identity or nonce")
	}
	if err := r.assertBinds(grant, req); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.grants[grant.GrantID]; exists {
		return Errorf(ErrorCodeApprovalInvalid, "grant %s already registered", grant.GrantID)
	}
	if _, consumed := r.consumedNonces[grant.Nonce]; consumed {
		return Errorf(ErrorCodeApprovalInvalid, "grant nonce already consumed")
	}
	r.grants[grant.GrantID] = registeredGrant{grant: *grant}
	return nil
}

// assertBinds verifies the grant's bound fields match req, so registration
// fails fast on a grant minted for a different request.
func (r *Registry) assertBinds(grant *approval.ApprovalGrant, req *SandboxRequest) error {
	payload, err := DigestPayload(req)
	if err != nil {
		return Errorf(ErrorCodeApprovalInvalid, "compute request digest: %v", err)
	}
	if grant.ArgumentsHash != approval.HashArguments(payload) {
		return Errorf(ErrorCodeApprovalInvalid, "grant does not bind this request digest")
	}
	if grant.CapabilitiesHash != approval.HashCapabilities(req.Capabilities) {
		return Errorf(ErrorCodeApprovalInvalid, "grant does not bind these capabilities")
	}
	if grant.PrincipalID != req.PrincipalID || grant.SessionID != req.SessionID || grant.ToolName != req.ToolName {
		return Errorf(ErrorCodeApprovalInvalid, "grant does not bind this principal, session, or tool")
	}
	return nil
}

// resolve validates the grant bound to req.ApprovalGrantID and consumes its
// one-shot nonce. It is the single chokepoint every elevated run passes
// before reaching the contained executor, so a replayed or altered request
// fails here regardless of the caller.
func (r *Registry) resolve(req *SandboxRequest) (approval.ApprovalGrant, error) {
	if req == nil {
		return approval.ApprovalGrant{}, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	if req.ApprovalGrantID == "" {
		return approval.ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "request carries no approval grant")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.grants[req.ApprovalGrantID]
	if !ok {
		return approval.ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "approval grant %s is not registered", req.ApprovalGrantID)
	}
	if _, consumed := r.consumedNonces[entry.grant.Nonce]; consumed {
		return approval.ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "approval grant nonce was already consumed")
	}
	payload, err := DigestPayload(req)
	if err != nil {
		return approval.ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "compute request digest: %v", err)
	}
	toolReq := &approval.ToolRequest{
		PrincipalID:  req.PrincipalID,
		SessionID:    req.SessionID,
		ToolName:     req.ToolName,
		Arguments:    payload,
		Capabilities: req.Capabilities,
	}
	// ValidFor re-checks principal, session, tool, argument digest,
	// capabilities, policy version, and expiry. Its typed error is remapped
	// onto the sandbox's own approval_invalid code so callers see one contract.
	if err := entry.grant.ValidFor(toolReq, r.policyVersion, r.now()); err != nil {
		return approval.ApprovalGrant{}, Errorf(ErrorCodeApprovalInvalid, "%v", err)
	}
	r.consumedNonces[entry.grant.Nonce] = struct{}{}
	return entry.grant, nil
}

// Execute resolves and consumes the request's approval grant, then runs the
// contained executable. A grant failure is reported before any child starts;
// a run outcome is recorded as one telemetry line carrying only non-secret
// identifying and accounting fields.
func (r *Registry) Execute(ctx context.Context, req *SandboxRequest) (Result, error) {
	grant, err := r.resolve(req)
	if err != nil {
		r.recordDenied(ctx, req, err)
		return Result{}, err
	}
	spec := specFromRequest(req)
	start := r.now()
	result, runErr := Run(ctx, spec, req.Executable, req.Arguments...)
	r.record(ctx, req, &grant, result, runErr, r.now().Sub(start))
	return result, runErr
}

func specFromRequest(req *SandboxRequest) *Spec {
	environment := make([]string, 0, len(req.Environment))
	for key, value := range req.Environment {
		environment = append(environment, key+"="+value)
	}
	return &Spec{
		WorkingDir:     req.WorkingDir,
		ReadOnlyPaths:  sortedClone(req.ReadOnlyPaths),
		ReadWritePaths: sortedClone(req.ReadWritePaths),
		AllowEnv:       environment,
		Limits:         req.Limits,
	}
}
