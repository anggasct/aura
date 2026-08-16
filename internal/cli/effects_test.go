package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
)

func seedUnknownEffect(t *testing.T, dataRoot string) (configPath, intentID string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.NewSessionService(db).Create(ctx, &store.Session{
		ID: "session-cli", OwnerID: "owner-cli", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	j, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	intent, err := j.Prepare(ctx, &effect.PrepareRequest{
		SessionID: "session-cli", TurnID: "turn-cli", ToolCallID: "call-cli", IdempotencyKey: "idem-cli",
		Provider: "telegram", Operation: "send_message", Classification: effect.ClassificationEffectful,
		Request: json.RawMessage(`{"chat":"owner","text":"hello"}`), EventSequence: 1,
		EventInvocation: "inv-cli", EventBranch: "main", EventAuthor: "agent",
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := j.Start(ctx, intent.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := j.MarkUnknown(ctx, intent.ID); err != nil {
		t.Fatalf("unknown: %v", err)
	}
	return filepath.Join(t.TempDir(), "config.yaml"), intent.ID
}

func TestEffectsListApproveAndMark(t *testing.T) {
	dataRoot := t.TempDir()
	cfg, id := seedUnknownEffect(t, dataRoot)
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("version: 1\nstorage:\n  path: %s\n", dataRoot)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out, err := execute(t, "effects", "list", "--config", cfg, "--state", "unknown")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "telegram") || strings.Contains(out, "hello") {
		t.Fatalf("unexpected safe list output: %s", out)
	}

	reason := "owner verified the external result"
	out, err = execute(t, "effects", "approve", id, "--config", cfg, "--action", "mark-failed", "--reason", reason)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	const tokenPrefix = "approval_token: "
	lineStart := strings.Index(out, tokenPrefix)
	if lineStart < 0 {
		t.Fatalf("approval output missing token: %s", out)
	}
	token := strings.TrimSpace(strings.Split(strings.TrimPrefix(out[lineStart:], tokenPrefix), "\n")[0])
	if token == "" {
		t.Fatal("approval token is empty")
	}

	out, err = execute(t, "effects", "mark", id, "--config", cfg, "--failed", "--reason", reason, "--approval-token", token)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !strings.Contains(out, "state: failed") {
		t.Fatalf("mark output = %s", out)
	}
}

func TestEffectsCommandRequiresExactApprovalInputs(t *testing.T) {
	dataRoot := t.TempDir()
	cfg, id := seedUnknownEffect(t, dataRoot)
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("version: 1\nstorage:\n  path: %s\n", dataRoot)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := execute(t, "effects", "approve", id, "--config", cfg, "--action", "retry"); err == nil || !strings.Contains(err.Error(), "requires --reason") {
		t.Fatalf("approve without reason error = %v", err)
	}
	if _, err := execute(t, "effects", "mark", id, "--config", cfg, "--failed", "--reason", "owner decision"); err == nil || !strings.Contains(err.Error(), "requires --approval-token") {
		t.Fatalf("mark without token error = %v", err)
	}
}
