package sandbox

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
)

func testRequest(t *testing.T) *SandboxRequest {
	t.Helper()
	return &SandboxRequest{
		RequestID:      "req-1",
		PrincipalID:    "principal-1",
		SessionID:      "session-1",
		ToolName:       "tool.exec.sandboxed",
		Executable:     "echo",
		Arguments:      []string{"hello"},
		WorkingDir:     t.TempDir(),
		ReadWritePaths: []string{t.TempDir()},
		Environment:    map[string]string{"PATH": "/usr/bin"},
		Capabilities:   []string{"tool.exec.sandboxed"},
		Limits:         Limits{Timeout: 5 * time.Second, MemoryBytes: 16 << 20},
	}
}

func cloneRequest(req *SandboxRequest) *SandboxRequest {
	clone := *req
	clone.Arguments = slices.Clone(req.Arguments)
	clone.ReadOnlyPaths = slices.Clone(req.ReadOnlyPaths)
	clone.ReadWritePaths = slices.Clone(req.ReadWritePaths)
	clone.Capabilities = slices.Clone(req.Capabilities)
	environment := make(map[string]string, len(req.Environment))
	for key, value := range req.Environment {
		environment[key] = value
	}
	clone.Environment = environment
	return &clone
}

func mintTestGrant(t *testing.T, policyVersion string, ttl time.Duration, req *SandboxRequest) approval.ApprovalGrant {
	t.Helper()
	payload, err := DigestPayload(req)
	if err != nil {
		t.Fatalf("DigestPayload: %v", err)
	}
	toolReq := &approval.ToolRequest{
		RequestID:    req.RequestID,
		PrincipalID:  req.PrincipalID,
		SessionID:    req.SessionID,
		ToolName:     req.ToolName,
		Arguments:    payload,
		Capabilities: req.Capabilities,
		Trust:        approval.TrustOwnerInput,
	}
	handler := func(context.Context, approval.ToolRequest, approval.Constraints) (approval.ToolResult, error) {
		return approval.ToolResult{}, nil
	}
	engine, err := approval.NewEngine(approval.Policy{
		Version: policyVersion,
		Rules:   map[string]approval.Rule{req.ToolName: {ToolName: req.ToolName, RequiresApproval: true}},
	}, handler)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	grant, err := engine.Grant(context.Background(), toolReq, ttl)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	return grant
}

func wantApprovalInvalid(t *testing.T, err error) {
	t.Helper()
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("err = %v, want approval_invalid", err)
	}
}

// TestRegisterAcceptsMatchingGrant proves the happy path: a grant minted over
// the request digests identically and registers, so a later resolve can bind it.
func TestRegisterAcceptsMatchingGrant(t *testing.T) {
	req := testRequest(t)
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// TestRegisterRejectsMismatchedGrant proves a grant minted for one request
// cannot be registered against another.
func TestRegisterRejectsMismatchedGrant(t *testing.T) {
	req := testRequest(t)
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register matching: %v", err)
	}
	other := cloneRequest(req)
	other.Arguments = []string{"tampered"}
	wantApprovalInvalid(t, registry.Register(context.Background(), &grant, other))

	grant.GrantID = ""
	wantApprovalInvalid(t, registry.Register(context.Background(), &grant, req))
}

// TestResolveMutationMatrix proves every altered bound field invalidates
// resolution. No failing case consumes the nonce, so the original request
// still resolves after the sweep.
func TestResolveMutationMatrix(t *testing.T) {
	req := testRequest(t)
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cases := map[string]func(*SandboxRequest){
		"argument":   func(r *SandboxRequest) { r.Arguments = []string{"hello", "extra"} },
		"executable": func(r *SandboxRequest) { r.Executable = "cat" },
		"workingdir": func(r *SandboxRequest) { r.WorkingDir = t.TempDir() },
		"path":       func(r *SandboxRequest) { r.ReadWritePaths = []string{t.TempDir()} },
		"readonly":   func(r *SandboxRequest) { r.ReadOnlyPaths = []string{t.TempDir()} },
		"limit":      func(r *SandboxRequest) { r.Limits.MemoryBytes = 32 << 20 },
		"timeout":    func(r *SandboxRequest) { r.Limits.Timeout = 10 * time.Second },
		"principal":  func(r *SandboxRequest) { r.PrincipalID = "principal-2" },
		"session":    func(r *SandboxRequest) { r.SessionID = "session-2" },
		"tool":       func(r *SandboxRequest) { r.ToolName = "tool.exec.other" },
		"capability": func(r *SandboxRequest) { r.Capabilities = []string{"tool.exec.other"} },
		"env":        func(r *SandboxRequest) { r.Environment["EXTRA"] = "v" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := cloneRequest(req)
			mutated.ApprovalGrantID = grant.GrantID
			mutate(mutated)
			_, err := registry.resolve(mutated)
			wantApprovalInvalid(t, err)
		})
	}

	// None of the rejected resolutions consumed the one-shot nonce, so the
	// original bound request still resolves exactly once.
	req.ApprovalGrantID = grant.GrantID
	if _, err := registry.resolve(req); err != nil {
		t.Fatalf("resolve original after sweep: %v", err)
	}
}

// TestResolveNonceOneShot proves a grant executes once and never again.
func TestResolveNonceOneShot(t *testing.T) {
	req := testRequest(t)
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req.ApprovalGrantID = grant.GrantID
	if _, err := registry.resolve(req); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	_, err := registry.resolve(req)
	wantApprovalInvalid(t, err)
}

// TestResolveUnregistered proves an unknown grant id never reaches the executor.
func TestResolveUnregistered(t *testing.T) {
	registry := NewRegistry("v1", nil)
	req := testRequest(t)
	req.ApprovalGrantID = "ghost"
	_, err := registry.resolve(req)
	wantApprovalInvalid(t, err)
}

// TestResolvePolicyVersionDrift proves reloading policy invalidates grants
// minted under the prior version.
func TestResolvePolicyVersionDrift(t *testing.T) {
	req := testRequest(t)
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	registry.policyVersion = "v2"
	req.ApprovalGrantID = grant.GrantID
	_, err := registry.resolve(req)
	wantApprovalInvalid(t, err)
}

// TestResolveExpiry proves an expired grant is refused even if its nonce is
// untouched. The grant expires one minute after minting; the registry clock is
// advanced past that window before resolution.
func TestResolveExpiry(t *testing.T) {
	req := testRequest(t)
	grant := mintTestGrant(t, "v1", time.Minute, req)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	registry.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	req.ApprovalGrantID = grant.GrantID
	_, err := registry.resolve(req)
	wantApprovalInvalid(t, err)
}

// TestTelemetryRedactsSecrets proves the run telemetry line carries the
// accounting fields an operator needs and never the request's arguments,
// output, or environment values.
func TestTelemetryRedactsSecrets(t *testing.T) {
	req := testRequest(t)
	req.Arguments = []string{"arg-SECRET-xyz"}
	req.Environment["TOKEN"] = "env-SECRET-abc"
	grant := mintTestGrant(t, "v1", time.Minute, req)
	var buf bytes.Buffer
	registry := NewRegistry("v1", slog.New(slog.NewTextHandler(&buf, nil)))
	if err := registry.Register(context.Background(), &grant, req); err != nil {
		t.Fatalf("Register: %v", err)
	}
	result := Result{Output: "out-SECRET-007", ExitCode: 0}
	registry.record(context.Background(), req, &grant, result, nil, 5*time.Millisecond)

	got := buf.String()
	for _, field := range []string{"request_id", "executable", "tool", "result_code", "duration", "memory_bytes", "truncated"} {
		if !strings.Contains(got, field) {
			t.Errorf("telemetry missing field %q:\n%s", field, got)
		}
	}
	for _, secret := range []string{"arg-SECRET-xyz", "env-SECRET-abc", "out-SECRET-007", "TOKEN"} {
		if strings.Contains(got, secret) {
			t.Errorf("telemetry leaked secret %q:\n%s", secret, got)
		}
	}
}
