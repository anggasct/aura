package connector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sandbox"
)

func TestConnectorSessionStarterAppliesTheConnectorProfileAndLimits(t *testing.T) {
	var got *sandbox.SessionRequest
	starter := NewSandboxStarter(func(_ context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error) {
		got = req
		return nil, errors.New("stop here")
	})
	req := &SessionRequest{
		RequestID:      "req-1",
		Name:           "mailer",
		Executable:     "/bundle/bin/connector",
		WorkingDir:     "/bundle",
		Timeout:        time.Minute,
		SetupTimeout:   20 * time.Second,
		MaxOutputBytes: 1 << 20,
	}
	if _, err := starter.StartSession(t.Context(), req); err == nil {
		t.Fatal("stub error swallowed")
	}
	if got == nil {
		t.Fatal("start function not called")
	}
	if got.Profile != sandbox.ProfileConnector {
		t.Errorf("profile = %q, want the connector profile", got.Profile)
	}
	want := sandbox.Limits{
		Timeout:        time.Minute,
		MaxOutputBytes: 1 << 20,
		MemoryBytes:    connectorMemoryBytes,
		MaxProcesses:   connectorMaxProcesses,
		MaxOpenFiles:   connectorMaxOpenFiles,
		FileBytes:      connectorFileBytes,
	}
	if got.Limits != want {
		t.Errorf("limits = %+v, want %+v", got.Limits, want)
	}
	if got.Limits.CPUTime != 0 {
		t.Errorf("cpu time = %v, a long-lived session must not carry a cpu limit", got.Limits.CPUTime)
	}
	if got.WorkingDir != "/bundle" || len(got.ReadWritePaths) != 0 {
		t.Errorf("paths = %q %v, want the bundle as working dir and no writable path", got.WorkingDir, got.ReadWritePaths)
	}
	if len(got.Environment) != 0 || got.Environment == nil {
		t.Errorf("environment = %#v, want empty non-nil map", got.Environment)
	}
	if len(got.Capabilities) != 0 || got.ApprovalGrantID != "" {
		t.Errorf("grant surface = %v %q, want empty", got.Capabilities, got.ApprovalGrantID)
	}
	if _, err := starter.StartSession(t.Context(), nil); !fault.HasCode(err, ErrConfigInvalid) {
		t.Errorf("StartSession(nil) = %v, want %s", err, ErrConfigInvalid)
	}
}

func TestConnectorSandboxAppliesProfileLimits(t *testing.T) {
	var got *sandbox.SessionRequest
	starter := NewSandboxStarter(func(_ context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error) {
		got = req
		return nil, errors.New("stop here")
	})
	_, _ = starter.StartSession(t.Context(), &SessionRequest{
		RequestID:      "req-2",
		Name:           "gmail",
		Executable:     "/bundle/bin/aura-connector-gmail",
		Arguments:      []string{"--stdio"},
		WorkingDir:     "/bundle",
		Timeout:        30 * time.Second,
		SetupTimeout:   10 * time.Second,
		MaxOutputBytes: 640 << 10,
	})
	if got == nil {
		t.Fatal("start function not called")
	}
	if got.Profile != sandbox.ProfileConnector {
		t.Fatalf("profile = %q, want %q", got.Profile, sandbox.ProfileConnector)
	}
	if got.Limits.MemoryBytes != connectorMemoryBytes ||
		got.Limits.MaxProcesses != connectorMaxProcesses ||
		got.Limits.MaxOpenFiles != connectorMaxOpenFiles ||
		got.Limits.FileBytes != connectorFileBytes {
		t.Fatalf("limits = %+v", got.Limits)
	}
}

func TestConnectorSandboxOutputExceededMapsToRPCLimit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"output exceeded", sandbox.Errorf(sandbox.ErrorCodeSandboxOutputExceeded, "too big"), ErrRPCLimit},
		{"session broken", sandbox.Errorf(sandbox.ErrorCodeSessionBroken, "gone"), ErrBackendUnavailable},
		{"timeout", sandbox.Errorf(sandbox.ErrorCodeSandboxTimeout, "slow"), ErrBackendUnavailable},
		{"unavailable", sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "no linux"), ErrBackendUnavailable},
		{"plain", errors.New("plain failure"), ErrBackendUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateSandboxError(tc.err)
			if !fault.HasCode(got, tc.want) {
				t.Fatalf("translateSandboxError(%v) = %v, want %s", tc.err, got, tc.want)
			}
		})
	}
	starter := NewSandboxStarter(func(context.Context, *sandbox.SessionRequest) (*sandbox.Session, error) {
		return nil, sandbox.Errorf(sandbox.ErrorCodeSandboxOutputExceeded, "oversized")
	})
	if _, err := starter.StartSession(t.Context(), &SessionRequest{Name: "x", Executable: "/bin/x", WorkingDir: "/tmp"}); !fault.HasCode(err, ErrRPCLimit) {
		t.Fatalf("StartSession output exceeded = %v, want %s", err, ErrRPCLimit)
	}
}
