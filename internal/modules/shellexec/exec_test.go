package shellexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestNewRejectsUnsafeOptions(t *testing.T) {
	cases := []Options{
		{Workspace: "relative", Timeout: time.Second, MaxStdoutBytes: 1, MaxStderrBytes: 1},
		{Workspace: t.TempDir(), MaxStdoutBytes: 1, MaxStderrBytes: 1},
		{Workspace: t.TempDir(), Timeout: time.Second, MaxStderrBytes: 1},
		{Workspace: t.TempDir(), Timeout: time.Second, MaxStdoutBytes: 1},
		{Workspace: t.TempDir(), Timeout: time.Second, MaxStdoutBytes: 1, MaxStderrBytes: 1, Environment: []string{"BAD-KEY=value"}},
	}
	for i := range cases {
		if _, err := NewAdapter(&cases[i]); err == nil {
			t.Errorf("NewAdapter(%+v) accepted invalid options", cases[i])
		}
	}
}

func TestAdapterRejectsRemoteCommandsBeforeSandbox(t *testing.T) {
	adapter, err := NewAdapter(&Options{Workspace: t.TempDir(), Timeout: time.Second, MaxStdoutBytes: 1024, MaxStderrBytes: 1024})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	for _, raw := range []string{`{"command":["ssh","host"]}`, `{"command":["ssh host"],"shell":true}`} {
		request := &toolbroker.ToolRequest{
			ToolName:    "exec",
			ToolVersion: "v1",
			Arguments:   json.RawMessage(raw),
		}
		_, err = adapter(context.Background(), request, approval.Constraints{})
		if err == nil || !strings.Contains(err.Error(), "remote command execution is not allowed") {
			t.Errorf("remote command %s err = %v, want remote command rejection", raw, err)
		}
	}
}

func TestCapOutput(t *testing.T) {
	if got := capOutput("abcdef", 3); got != "abc" {
		t.Fatalf("capOutput = %q, want abc", got)
	}
	if got := capOutput("abc", 3); got != "abc" {
		t.Fatalf("capOutput preserved value = %q", got)
	}
}
