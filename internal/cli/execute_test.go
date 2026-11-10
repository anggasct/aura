package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/app"
)

func TestExecuteContextExitCodes(t *testing.T) {
	if code := ExecuteContext(context.Background(), nil, "version"); code != 0 {
		t.Errorf("version exit code = %d, want 0", code)
	}
	if code := ExecuteContext(context.Background(), nil, "exec"); code != 2 {
		t.Errorf("usage error exit code = %d, want 2", code)
	}
	if code := ExecuteContext(context.Background(), nil, "bogus"); code != 2 {
		t.Errorf("unknown command exit code = %d, want 2", code)
	}
	if code := ExecuteContext(context.Background(), nil, "help"); code != 0 {
		t.Errorf("built-in help exit code = %d, want 0", code)
	}
	if code := ExecuteContext(context.Background(), nil, "help", "version"); code != 0 {
		t.Errorf("built-in help <cmd> exit code = %d, want 0", code)
	}
	if code := ExecuteContext(context.Background(), nil, "completion", "bash"); code != 0 {
		t.Errorf("built-in completion exit code = %d, want 0", code)
	}
	if code := ExecuteContext(context.Background(), nil, "server", "--config", "definitely-missing.yaml"); code != 1 {
		t.Errorf("runtime error exit code = %d, want 1", code)
	}
}

func TestExecuteContextBuiltinsViaProcessArgs(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"aura", "help"}
	if code := ExecuteContext(context.Background(), nil); code != 0 {
		t.Errorf("help (process args) exit code = %d, want 0", code)
	}
	os.Args = []string{"aura", "completion", "bash"}
	if code := ExecuteContext(context.Background(), nil); code != 0 {
		t.Errorf("completion bash (process args) exit code = %d, want 0", code)
	}
}

func TestExecuteContextUnknownCommandViaProcessArgs(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"aura", "bogus"}
	if code := ExecuteContext(context.Background(), nil); code != 2 {
		t.Errorf("unknown command (process args) exit code = %d, want 2", code)
	}
	os.Args = []string{"aura", "--config", "x", "bogus"}
	if code := ExecuteContext(context.Background(), nil); code != 2 {
		t.Errorf("unknown command after flags (process args) exit code = %d, want 2", code)
	}
}

func TestContextForCommandCancelsOnInterrupt(t *testing.T) {
	signals := make(chan os.Signal, 1)
	ctx := context.Background()
	executionCtx, stop := contextForCommand(ctx, app.NewRootCommand(), []string{"server"}, signals)
	signals <- os.Interrupt
	select {
	case <-executionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("non-chat command context was not cancelled")
	}
	stop()
}

func TestContextForChatLeavesInterruptsForConsole(t *testing.T) {
	signals := make(chan os.Signal, 1)
	ctx := context.Background()
	executionCtx, stop := contextForCommand(ctx, app.NewRootCommand(), []string{"chat"}, signals)
	defer stop()
	signals <- os.Interrupt
	select {
	case <-executionCtx.Done():
		t.Fatal("chat context cancelled by process interrupt bridge")
	default:
	}
}
