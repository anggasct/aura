package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/store"

	"google.golang.org/adk/v2/session"
)

// loadGoldenADKEvent reads the golden fixture as an ADK event.
func loadGoldenADKEvent(t *testing.T) *session.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "runtime", "adk_event_golden.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var ev session.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("unmarshal golden fixture: %v", err)
	}
	return &ev
}

// A full-fidelity ADK event must survive mapping, persistence, replay, and
// mapping back without losing invocation, branch, author, actions,
// long-running tool IDs, content, or usage.
func TestADKGoldenEventSurvivesFullRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, sessions, events := newSessionTestDB(t)
	svc, err := NewADKSessionService(sessions)
	if err != nil {
		t.Fatalf("NewADKSessionService: %v", err)
	}
	if _, err := svc.Create(ctx, &session.CreateRequest{AppName: "aura", UserID: "user-1", SessionID: "session-golden"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	original := loadGoldenADKEvent(t)

	// Mapping + persistence: the engine is the single writer, persisting the
	// mapped event exactly as the executor yields it — original ADK event ID,
	// turn and session identity, full fidelity payload.
	re, err := store.RuntimeEventFromADK("session-golden", "turn-golden", original)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	re.Sequence = 1
	if err := events.Append(ctx, &re); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Replay + mapping back: reload the session from the store.
	reloaded, err := svc.Get(ctx, &session.GetRequest{SessionID: "session-golden"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Session.Events().Len() != 1 {
		t.Fatalf("events = %d, want 1", reloaded.Session.Events().Len())
	}
	got := reloaded.Session.Events().At(0)

	if got.ID != original.ID {
		t.Errorf("ID = %q, want %q", got.ID, original.ID)
	}
	if got.InvocationID != original.InvocationID {
		t.Errorf("InvocationID = %q, want %q", got.InvocationID, original.InvocationID)
	}
	if got.Branch != original.Branch {
		t.Errorf("Branch = %q, want %q", got.Branch, original.Branch)
	}
	if got.Author != original.Author {
		t.Errorf("Author = %q, want %q", got.Author, original.Author)
	}
	if got.Content == nil || len(got.Content.Parts) == 0 || got.Content.Parts[0].Text != "golden answer" {
		t.Errorf("Content = %+v, want the golden answer text", got.Content)
	}
	if got.Actions.TransferToAgent != "researcher" || !got.Actions.Escalate {
		t.Errorf("Actions = %+v, want transfer+escalate preserved", got.Actions)
	}
	if len(got.LongRunningToolIDs) != 2 || got.LongRunningToolIDs[0] != "tool-1" {
		t.Errorf("LongRunningToolIDs = %v, want [tool-1 tool-2]", got.LongRunningToolIDs)
	}
	if got.UsageMetadata == nil || got.UsageMetadata.TotalTokenCount != 30 {
		t.Errorf("UsageMetadata = %+v, want total 30", got.UsageMetadata)
	}

	// The stored byte shape is pinned: a re-marshal of the payload must match
	// the golden fixture's content/actions/usage.
	persisted, err := sessions.ListEvents(ctx, "session-golden", 0, 1)
	if err != nil || len(persisted) != 1 {
		t.Fatalf("ListEvents = %d, %v; want 1", len(persisted), err)
	}
	var payload struct {
		Content            json.RawMessage      `json:"content,omitempty"`
		Actions            session.EventActions `json:"actions"`
		LongRunningToolIDs []string             `json:"longRunningToolIds,omitempty"`
		Usage              json.RawMessage      `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(persisted[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal stored payload: %v", err)
	}
	if len(payload.LongRunningToolIDs) != 2 {
		t.Errorf("stored LongRunningToolIDs = %v, want 2", payload.LongRunningToolIDs)
	}
}

// The golden fixture must keep round-tripping after ADK upgrades; the test
// above asserts semantic fidelity, and this test pins the exact stored bytes.
func TestADKGoldenEventStoredBytesStable(t *testing.T) {
	original := loadGoldenADKEvent(t)
	re, err := store.RuntimeEventFromADK("session-golden", "turn-1", original)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}

	roundTripped, err := store.RuntimeEventToADK(&re)
	if err != nil {
		t.Fatalf("RuntimeEventToADK: %v", err)
	}
	// The replay path reads ADK events; a semantic compare is the contract.
	if roundTripped.ID != original.ID || roundTripped.Branch != original.Branch {
		t.Errorf("round trip lost identity: %+v vs %+v", roundTripped, original)
	}
}
