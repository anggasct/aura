package app

import (
	"bytes"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), err
}

func exitCodeOf(t *testing.T, args ...string) int {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	return ExitCode(root, args, root.Execute())
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

func TestChatWithoutModelFailsLoudly(t *testing.T) {
	_, err := execute(t, "chat", "--config", "../../testdata/config/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected chat to fail loudly, got nil")
	}
	if strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("chat is implemented; got stale stub message: %v", err)
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Errorf("expected chat to fail at config load, got: %v", err)
	}
}

func TestExecStub(t *testing.T) {
	_, err := execute(t, "exec", "--", "ls", "-la")
	if err == nil {
		t.Fatal("expected exec to fail loudly, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("unexpected exec error: %v", err)
	}
}

func TestExecRequiresArgument(t *testing.T) {
	_, err := execute(t, "exec")
	if err == nil {
		t.Fatal("expected usage error for exec without arguments, got nil")
	}
	if !strings.Contains(err.Error(), "requires at least one argument") {
		t.Errorf("unexpected exec usage error: %v", err)
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
