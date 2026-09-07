package sandbox

import (
	"context"
	"testing"
	"time"
)

func testSessionRequest(t *testing.T) *SessionRequest {
	t.Helper()
	return &SessionRequest{
		RequestID:   "session-req-1",
		SessionID:   "session-1",
		Executable:  "helper",
		WorkingDir:  t.TempDir(),
		Environment: map[string]string{"PATH": "/usr/bin"},
		Limits:      Limits{Timeout: 5 * time.Second},
	}
}

// cloneSessionRequest copies a session request deeply enough for validation
// tests: the bound environment map and capability slice must not be shared
// with the original, or a mutation leaks back into it.
func cloneSessionRequest(req *SessionRequest) *SessionRequest {
	clone := *req
	clone.Arguments = append([]string(nil), req.Arguments...)
	clone.ReadOnlyPaths = append([]string(nil), req.ReadOnlyPaths...)
	clone.ReadWritePaths = append([]string(nil), req.ReadWritePaths...)
	clone.Capabilities = append([]string(nil), req.Capabilities...)
	clone.Environment = make(map[string]string, len(req.Environment))
	for key, value := range req.Environment {
		clone.Environment[key] = value
	}
	return &clone
}

func TestStartValidatesRequest(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*SessionRequest)
	}{
		{"empty workdir", func(r *SessionRequest) { r.WorkingDir = " " }},
		{"empty executable", func(r *SessionRequest) { r.Executable = " " }},
		{"zero timeout", func(r *SessionRequest) { r.Limits.Timeout = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testSessionRequest(t)
			tc.mut(req)
			_, err := Start(t.Context(), req)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("Start = %v, want invalid_argument", err)
			}
		})
	}
}

// TestStartNilRequest proves the pointer contract: a nil request is a typed
// invalid_argument, never a panic.
func TestStartNilRequest(t *testing.T) {
	_, err := Start(t.Context(), nil)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Start(nil) = %v, want invalid_argument", err)
	}
}

// TestStartRejectsGrantWithoutValidator proves the fail-closed grant
// contract: with no validator authority configured, a request that carries
// an ApprovalGrantID is refused before any child is spawned — the field can
// never be silently ignored.
func TestStartRejectsGrantWithoutValidator(t *testing.T) {
	req := testSessionRequest(t)
	req.ApprovalGrantID = "ghost"
	_, err := Start(t.Context(), req)
	wantApprovalInvalid(t, err)
}

// TestValidateSessionGrantResolution drives the Registry-backed seam
// directly: a registered grant that binds the session request validates, an
// unregistered grant ID is refused, a consumed nonce is refused, and every
// altered bound field — the same mutation matrix as the one-shot resolve —
// invalidates validation without consuming the one-shot nonce.
func TestValidateSessionGrantResolution(t *testing.T) {
	req := &SessionRequest{
		RequestID:      "session-req-1",
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
	contract, err := sessionContract(req)
	if err != nil {
		t.Fatalf("sessionContract: %v", err)
	}
	grant := mintTestGrant(t, "v1", time.Minute, contract)
	registry := NewRegistry("v1", nil)
	if err := registry.Register(context.Background(), &grant, contract); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req.ApprovalGrantID = grant.GrantID
	if err := registry.validateSessionGrant(req); err != nil {
		t.Fatalf("validateSessionGrant(bound): %v", err)
	}

	// Unregistered grant ID.
	ghost := testSessionRequest(t)
	ghost.ApprovalGrantID = "ghost"
	wantApprovalInvalid(t, registry.validateSessionGrant(ghost))

	// Missing grant ID under an enforcing validator.
	empty := testSessionRequest(t)
	wantApprovalInvalid(t, registry.validateSessionGrant(empty))

	// Mutation matrix: every altered bound field fails validation.
	mutations := map[string]func(*SessionRequest){
		"argument":   func(r *SessionRequest) { r.Arguments = []string{"hello", "extra"} },
		"executable": func(r *SessionRequest) { r.Executable = "cat" },
		"workingdir": func(r *SessionRequest) { r.WorkingDir = t.TempDir() },
		"path":       func(r *SessionRequest) { r.ReadWritePaths = []string{t.TempDir()} },
		"principal":  func(r *SessionRequest) { r.PrincipalID = "principal-2" },
		"session":    func(r *SessionRequest) { r.SessionID = "session-2" },
		"tool":       func(r *SessionRequest) { r.ToolName = "tool.exec.other" },
		"capability": func(r *SessionRequest) { r.Capabilities = []string{"tool.exec.other"} },
		"env":        func(r *SessionRequest) { r.Environment["EXTRA"] = "v" },
		"limit":      func(r *SessionRequest) { r.Limits.MemoryBytes = 32 << 20 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutated := cloneSessionRequest(req)
			mutate(mutated)
			wantApprovalInvalid(t, registry.validateSessionGrant(mutated))
		})
	}

	// Policy-version drift and expiry invalidate session grants the same way.
	registry.policyVersion = "v2"
	wantApprovalInvalid(t, registry.validateSessionGrant(req))
	registry.policyVersion = "v1"
	registry.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	wantApprovalInvalid(t, registry.validateSessionGrant(req))
	registry.now = time.Now

	// None of the failed validations consumed the one-shot nonce: the
	// one-shot path can still spend the grant exactly once afterwards.
	contract.ApprovalGrantID = grant.GrantID
	if _, err := registry.resolve(contract); err != nil {
		t.Fatalf("one-shot resolve after session validations: %v", err)
	}
	// And once spent, the session refuses to start on the spent grant.
	wantApprovalInvalid(t, registry.validateSessionGrant(req))
}
