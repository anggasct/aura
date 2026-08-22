package exec

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func TestNewRejectsUnsafeOptions(t *testing.T) {
	cases := []Options{
		{Workspace: "relative", Timeout: time.Second, MaxStdoutBytes: 1, MaxStderrBytes: 1},
		{Workspace: t.TempDir(), MaxStdoutBytes: 1, MaxStderrBytes: 1},
		{Workspace: t.TempDir(), Timeout: time.Second, MaxStderrBytes: 1},
		{Workspace: t.TempDir(), Timeout: time.Second, MaxStdoutBytes: 1},
		{Workspace: t.TempDir(), Timeout: time.Second, MaxStdoutBytes: 1, MaxStderrBytes: 1, Environment: []string{"BAD-KEY=value"}},
	}
	for _, options := range cases {
		if _, err := New(options); err == nil {
			t.Errorf("New(%+v) accepted invalid options", options)
		}
	}
}

func TestAdapterRejectsRemoteCommandsBeforeSandbox(t *testing.T) {
	adapter, err := New(Options{Workspace: t.TempDir(), Timeout: time.Second, MaxStdoutBytes: 1024, MaxStderrBytes: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, raw := range []string{`{"command":["ssh","host"]}`, `{"command":["ssh host"],"shell":true}`} {
		request := &toolbroker.ToolRequest{
			ToolName:    "exec",
			ToolVersion: "v1",
			Arguments:   json.RawMessage(raw),
		}
		_, err = adapter(context.Background(), request, approval.Constraints{})
		if err == nil {
			t.Errorf("remote command %s was accepted", raw)
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
