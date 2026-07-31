package cli

import (
	"bytes"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), err
}

func TestVersionCommand(t *testing.T) {
	out, err := execute(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	for _, want := range []string{"aura dev", "commit:", "built:", "go:", "platform:", "profile:  core", "capabilities: none"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q:\n%s", want, out)
		}
	}
}

func TestChatStub(t *testing.T) {
	out, err := execute(t, "chat")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(out, "aura chat: interactive console not yet implemented") {
		t.Errorf("unexpected chat output:\n%s", out)
	}
}

func TestExecStub(t *testing.T) {
	out, err := execute(t, "exec", "--", "ls", "-la")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "aura exec: sandboxed execution not yet implemented") {
		t.Errorf("unexpected exec output:\n%s", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	if _, err := execute(t, "bogus"); err == nil {
		t.Fatal("expected error for unknown command, got nil")
	}
}

func TestServerInvalidConfig(t *testing.T) {
	_, err := execute(t, "server", "--config", "../../testdata/config/unknown_keys.yaml")
	if err == nil {
		t.Fatal("expected config error from server, got nil")
	}
	if !strings.Contains(err.Error(), `unknown key "server.typo"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestServerMissingExplicitConfig(t *testing.T) {
	_, err := execute(t, "server", "--config", "../../testdata/config/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected file-not-found error from server, got nil")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Errorf("unexpected error: %v", err)
	}
}
