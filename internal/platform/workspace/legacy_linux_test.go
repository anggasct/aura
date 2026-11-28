//go:build linux

package workspace

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type turnStoreLegacyEventReader struct {
	db *sql.DB
}

func (r turnStoreLegacyEventReader) ListSessionEventsFrom(ctx context.Context, sessionID string, fromSeq uint64) ([]LegacyEvent, error) {
	events, err := turnstore.ListSessionEventsFrom(ctx, r.db, sessionID, fromSeq, []string{turnstore.EventKindADK, "tool.completed"})
	if err != nil {
		return nil, err
	}
	out := make([]LegacyEvent, len(events))
	for i := range events {
		out[i] = LegacyEvent{Kind: events[i].Kind, Payload: events[i].Payload}
	}
	return out, nil
}

func appendADKEvent(t *testing.T, events turnstore.EventStore, sessID, turnID, id string, content *genai.Content) {
	t.Helper()
	ev, err := turnstore.RuntimeEventFromADK(sessID, turnID, &session.Event{
		ID:          id,
		Author:      "aura",
		Timestamp:   time.Now().UTC(),
		LLMResponse: adkmodel.LLMResponse{Content: content},
	})
	if err != nil {
		t.Fatalf("RuntimeEventFromADK %s: %v", id, err)
	}
	if _, err := events.AppendSequenced(t.Context(), sessID, &ev); err != nil {
		t.Fatalf("AppendSequenced %s: %v", id, err)
	}
}

func TestLegacyWorkspaceWritesAreRecorded(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte("external summary"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "denied.txt"), []byte("clean"), 0o600); err != nil {
		t.Fatalf("write denied fixture: %v", err)
	}

	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), Schema())
	sessions := turnstore.NewSessionService(db)
	events := turnstore.NewEventStore(db)
	now := time.Now().UTC()
	if err := sessions.Create(ctx, &turn.Session{ID: "sess-legacy", OwnerID: principal.OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-1", &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID:       "c1",
			Name:     "web_fetch",
			Response: map[string]any{"body": "from web"},
		}}},
	})
	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-2", &genai.Content{
		Role: "model",
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID:   "c2",
			Name: "write_file",
			Args: map[string]any{"path": "report.txt", "content": "external summary"},
		}}},
	})
	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-3", &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID:       "c2",
			Name:     "write_file",
			Response: map[string]any{"path": "report.txt", "bytes": 16},
		}}},
	})
	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-4", &genai.Content{
		Role: "model",
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID:   "c3",
			Name: "write_file",
			Args: map[string]any{"path": "denied.txt", "content": "bad"},
		}}},
	})
	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-5", &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID:       "c3",
			Name:     "write_file",
			Response: map[string]any{"error": map[string]any{"code": "policy_denied"}},
		}}},
	})
	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-6", &genai.Content{
		Role: "model",
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID:   "c4",
			Name: "exec",
			Args: map[string]any{"command": []any{"/bin/sh", "-c", "touch x"}},
		}}},
	})
	appendADKEvent(t, events, "sess-legacy", "turn-1", "ev-7", &genai.Content{
		Role: "user",
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			ID:       "c4",
			Name:     "exec",
			Response: map[string]any{"error": map[string]any{"code": "approval_denied"}},
		}}},
	})

	marked, err := turnstore.MarkLegacySessions(ctx, db, nil)
	if err != nil || len(marked) != 1 {
		t.Fatalf("MarkLegacySessions = %+v %v, want 1", marked, err)
	}
	legacy := []LegacySession{{SessionID: marked[0].SessionID, FirstTaintedSeq: marked[0].AtSequence}}
	recorded, err := DeriveLegacyWorkspace(ctx, db, turnStoreLegacyEventReader{db: db}, root, legacy)
	if err != nil || recorded != 1 {
		t.Fatalf("DeriveLegacyWorkspace = %d %v, want 1", recorded, err)
	}

	store := NewTaintStore(db)
	rows, err := store.List(ctx, "")
	if err != nil || len(rows) != 1 || rows[0].Path != "report.txt" || rows[0].Scope != ScopeFile || rows[0].Source != "legacy" {
		t.Fatalf("workspace_taint rows = %+v %v, want only 1 legacy file row for report.txt", rows, err)
	}

	if _, err := store.DeleteIDs(ctx, []string{rows[0].ID}); err != nil {
		t.Fatalf("DeleteIDs: %v", err)
	}
	markedAgain, err := turnstore.MarkLegacySessions(ctx, db, nil)
	if err != nil || len(markedAgain) != 0 {
		t.Fatalf("second MarkLegacySessions = %+v %v, want 0 (untaint stays cleared)", markedAgain, err)
	}
}
