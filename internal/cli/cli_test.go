package cli

import (
	"bytes"
	"context"
	"os"
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
	_, err := execute(t, "chat")
	if err == nil {
		t.Fatal("expected chat to fail loudly, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("unexpected chat error: %v", err)
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

func TestExecuteContextExitCodes(t *testing.T) {
	if code := ExecuteContext(context.Background(), "version"); code != 0 {
		t.Errorf("version exit code = %d, want 0", code)
	}
	if code := ExecuteContext(context.Background(), "exec"); code != 2 {
		t.Errorf("usage error exit code = %d, want 2", code)
	}
	if code := ExecuteContext(context.Background(), "bogus"); code != 2 {
		t.Errorf("unknown command exit code = %d, want 2", code)
	}
	if code := ExecuteContext(context.Background(), "server", "--config", "definitely-missing.yaml"); code != 1 {
		t.Errorf("runtime error exit code = %d, want 1", code)
	}
}

func TestExecuteContextUnknownCommandViaProcessArgs(t *testing.T) {
	// main calls ExecuteContext without explicit args; the effective args
	// come from os.Args, and the unknown-command classification must still
	// fire on that path.
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"aura", "bogus"}
	if code := ExecuteContext(context.Background()); code != 2 {
		t.Errorf("unknown command (process args) exit code = %d, want 2", code)
	}
	os.Args = []string{"aura", "--config", "x", "bogus"}
	if code := ExecuteContext(context.Background()); code != 2 {
		t.Errorf("unknown command after flags (process args) exit code = %d, want 2", code)
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
