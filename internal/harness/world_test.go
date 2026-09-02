package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
)

func TestComposedEntryVerifiesExternalWorldEvidence(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "marker.txt")
	if err := os.WriteFile(marker, []byte("stable"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := digestFile(t, marker)

	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := store.NewSessionService(db).Create(ctx, &store.Session{ID: "world-session", OwnerID: "owner"}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	events := store.NewEventStore(db)
	engine, err := runtimeengine.NewEngine(runtimeengine.Config{}, events, store.NewDedupeStore(db), runtime.NewFakeExecutor([]runtime.FakeStep{{Kind: runtime.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)}}), nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	for event, runErr := range engine.Run(ctx, &runtime.TurnRequest{
		TurnID: "world-turn", SessionID: "world-session", PrincipalID: "owner", Origin: runtime.OriginInternal,
		Parts: []runtimeingress.InputPart{{Text: "world"}},
	}) {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
		if event.Kind == runtime.EventKindTurnCompleted {
			break
		}
	}

	journal, err := effect.NewJournal(db, effect.Options{})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	sequence, err := events.LastSequence(ctx, "world-session")
	if err != nil {
		t.Fatalf("LastSequence: %v", err)
	}
	intent, err := journal.Prepare(ctx, &effect.PrepareRequest{
		SessionID: "world-session", TurnID: "world-turn", ToolCallID: "world-tool", IdempotencyKey: "world-key",
		Provider: "world-provider", Operation: "read", Classification: effect.ClassificationEffectful,
		Request: json.RawMessage(`{"path":"marker.txt"}`), EventID: "world-effect-request", EventSequence: sequence + 1,
		EventInvocation: "world-run", EventBranch: "main", EventAuthor: "runtime",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := journal.Start(ctx, intent.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	recovery, err := journal.Recover(ctx)
	if err != nil || recovery.Claimed != 1 {
		t.Fatalf("Recover report=%+v err=%v, want one ambiguous effect", recovery, err)
	}
	unknown, err := journal.ListByState(ctx, effect.StateUnknown, 10)
	if err != nil || len(unknown) != 1 {
		t.Fatalf("unknown effects=%d err=%v, want one recovered effect", len(unknown), err)
	}

	cmd := exec.CommandContext(ctx, "true")
	if err := cmd.Run(); err != nil || cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("process evidence err=%v state=%v", err, cmd.ProcessState)
	}
	if got := digestFile(t, marker); got != before {
		t.Fatalf("marker digest changed from %q to %q", before, got)
	}
	stored, err := store.NewSessionService(db).ListEvents(ctx, "world-session", 0, 100)
	if err != nil || len(stored) == 0 || stored[len(stored)-1].Kind != effect.EventKindToolRequested {
		t.Fatalf("stored world events=%d last=%q err=%v, want durable effect request", len(stored), lastEventKind(stored), err)
	}
	_, err = egress.Validate(ctx, "https://internal.example", compositionResolver{"internal.example": {net.ParseIP("10.0.0.3")}})
	if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
		t.Fatalf("unauthorized network validation = %v, code=%q ok=%v", err, code, ok)
	}
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func lastEventKind(events []store.RuntimeEvent) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Kind
}
