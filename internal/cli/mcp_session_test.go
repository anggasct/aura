package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/mcp"
	"github.com/anggasct/aura/internal/sandbox"
)

func TestMCPSessionStarterMapping(t *testing.T) {
	var got *sandbox.SessionRequest
	starter := newMCPSessionStarter(func(_ context.Context, req *sandbox.SessionRequest) (*sandbox.Session, error) {
		got = req
		return nil, errors.New("stop here")
	})
	req := &mcp.ContainedSessionRequest{
		RequestID: "req-1", ServerName: "docs", Executable: "/bin/server",
		Arguments: []string{"--stdio"}, WorkingDir: "/work",
		Environment: map[string]string{"SERVER_MODE": "test"},
		Timeout:     time.Minute, MaxOutputBytes: 1 << 20,
	}
	if _, err := starter.StartSession(t.Context(), req); err == nil {
		t.Fatal("stub error swallowed")
	}
	if got == nil {
		t.Fatal("start function not called")
	}
	if got.Executable != "/bin/server" || len(got.Arguments) != 1 || got.Arguments[0] != "--stdio" {
		t.Errorf("command = %+v", got)
	}
	if got.WorkingDir != "/work" {
		t.Errorf("working dir = %q", got.WorkingDir)
	}
	if !reflect.DeepEqual(got.Environment, map[string]string{"SERVER_MODE": "test"}) {
		t.Errorf("environment = %v", got.Environment)
	}
	if got.Limits.Timeout != time.Minute || got.Limits.MaxOutputBytes != 1<<20 {
		t.Errorf("limits = %+v", got.Limits)
	}
	if len(got.Capabilities) != 0 || got.ApprovalGrantID != "" {
		t.Errorf("grant surface = %v %q, want empty", got.Capabilities, got.ApprovalGrantID)
	}
	if _, err := starter.StartSession(t.Context(), nil); err == nil {
		t.Error("nil request accepted")
	} else if code, ok := mcp.CodeOf(err); !ok || code != mcp.ErrConfigInvalid {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestTranslateSessionError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code mcp.ErrorCode
	}{
		{sandbox.Errorf(sandbox.ErrorCodeSandboxOutputExceeded, "too big"), mcp.ErrMessageTooLarge},
		{sandbox.Errorf(sandbox.ErrorCodeSessionBroken, "gone"), mcp.ErrServerUnavailable},
		{sandbox.Errorf(sandbox.ErrorCodeSandboxTimeout, "slow"), mcp.ErrServerUnavailable},
		{sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "no linux"), mcp.ErrServerUnavailable},
		{errors.New("plain failure"), mcp.ErrServerUnavailable},
	} {
		err := translateSessionError(tc.err)
		if err == nil {
			t.Fatalf("error swallowed: %v", tc.err)
		}
		if code, ok := mcp.CodeOf(err); !ok || code != tc.code {
			t.Errorf("code = %v, %v, want %v", code, ok, tc.code)
		}
	}
}
