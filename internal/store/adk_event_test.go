package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestADKEventRoundTripPreservesRequiredFields(t *testing.T) {
	original := &session.Event{
		ID:           "event-1",
		Timestamp:    time.Now().UTC().Truncate(time.Millisecond),
		InvocationID: "invocation-1",
		Branch:       "agent_1.agent_2",
		Author:       "model",
		Actions: session.EventActions{
			StateDelta:      map[string]any{"k": "v"},
			ArtifactDelta:   map[string]int64{"file.txt": 1},
			TransferToAgent: "sub-agent",
			Escalate:        true,
		},
		LongRunningToolIDs: []string{"tool-call-1", "tool-call-2"},
	}
	original.Content = genai.NewContentFromText("final answer", genai.RoleModel)
	original.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     10,
		CandidatesTokenCount: 20,
		TotalTokenCount:      30,
	}

	re, err := RuntimeEventFromADK("session-1", "turn-1", original)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	re.Sequence = 1

	roundTripped, err := RuntimeEventToADK(re)
	if err != nil {
		t.Fatalf("RuntimeEventToADK: %v", err)
	}

	if roundTripped.InvocationID != original.InvocationID {
		t.Errorf("InvocationID = %q, want %q", roundTripped.InvocationID, original.InvocationID)
	}
	if roundTripped.Branch != original.Branch {
		t.Errorf("Branch = %q, want %q", roundTripped.Branch, original.Branch)
	}
	if roundTripped.Author != original.Author {
		t.Errorf("Author = %q, want %q", roundTripped.Author, original.Author)
	}
	if !reflect.DeepEqual(roundTripped.Actions, original.Actions) {
		t.Errorf("Actions = %+v, want %+v", roundTripped.Actions, original.Actions)
	}
	if !reflect.DeepEqual(roundTripped.LongRunningToolIDs, original.LongRunningToolIDs) {
		t.Errorf("LongRunningToolIDs = %v, want %v", roundTripped.LongRunningToolIDs, original.LongRunningToolIDs)
	}
	if !reflect.DeepEqual(roundTripped.Content, original.Content) {
		t.Errorf("Content = %+v, want %+v", roundTripped.Content, original.Content)
	}
	if !reflect.DeepEqual(roundTripped.UsageMetadata, original.UsageMetadata) {
		t.Errorf("UsageMetadata = %+v, want %+v", roundTripped.UsageMetadata, original.UsageMetadata)
	}
}

func TestReplayTurnReturnsTerminalEvent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	thinking := &session.Event{
		ID:           "event-thinking",
		Timestamp:    time.Now().UTC(),
		InvocationID: "invocation-1",
		Author:       "model",
	}
	thinking.Content = genai.NewContentFromText("thinking...", genai.RoleModel)
	thinking.Partial = true // not a final response

	final := &session.Event{
		ID:           "event-final",
		Timestamp:    time.Now().UTC(),
		InvocationID: "invocation-1",
		Author:       "model",
	}
	final.Content = genai.NewContentFromText("final answer", genai.RoleModel)

	for i, ev := range []*session.Event{thinking, final} {
		re, err := RuntimeEventFromADK("session-1", "turn-1", ev)
		if err != nil {
			t.Fatalf("RuntimeEventFromADK: %v", err)
		}
		re.Sequence = uint64(i + 1)
		if err := events.Append(ctx, re); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	terminal, found, err := ReplayTurn(ctx, db, "session-1", "turn-1")
	if err != nil {
		t.Fatalf("ReplayTurn: %v", err)
	}
	if !found {
		t.Fatal("ReplayTurn found = false, want true")
	}
	if terminal.ID != "event-final" {
		t.Errorf("terminal.ID = %s, want event-final", terminal.ID)
	}
}

func TestReplayTurnNotYetFinal(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	events := NewEventStore(db)

	thinking := &session.Event{ID: "event-thinking", Timestamp: time.Now().UTC(), InvocationID: "invocation-1", Author: "model"}
	thinking.Content = genai.NewContentFromText("still thinking", genai.RoleModel)
	thinking.Partial = true

	re, err := RuntimeEventFromADK("session-1", "turn-1", thinking)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	re.Sequence = 1
	if err := events.Append(ctx, re); err != nil {
		t.Fatalf("Append: %v", err)
	}

	_, found, err := ReplayTurn(ctx, db, "session-1", "turn-1")
	if err != nil {
		t.Fatalf("ReplayTurn: %v", err)
	}
	if found {
		t.Fatal("ReplayTurn found = true, want false (turn not yet final)")
	}
}
