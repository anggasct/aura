package profile

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

func testUserEvent(t *testing.T, id string, sequence uint64, parts ...string) turn.Event {
	t.Helper()
	type part struct {
		Text string `json:"text"`
	}
	content := struct {
		Content struct {
			Parts []part `json:"parts"`
		} `json:"content"`
	}{}
	for _, text := range parts {
		content.Content.Parts = append(content.Content.Parts, part{Text: text})
	}
	payload, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return turn.Event{ID: id, Sequence: sequence, Author: "user", Kind: "adk_event", Payload: payload}
}

func testModelEvent(t *testing.T, id string, sequence uint64, text string) turn.Event {
	t.Helper()
	event := testUserEvent(t, id, sequence, text)
	event.Author = "aura"
	return event
}

func TestProfileJobTakesOnlyOwnerWords(t *testing.T) {
	partial := testUserEvent(t, "evt-partial", 5, "i use emacs")
	partial.Payload = json.RawMessage(strings.Replace(string(partial.Payload), `{"content"`, `{"partial":true,"content"`, 1))
	events := []turn.Event{
		testUserEvent(t, "evt-user", 1, "i always use neovim", turn.RecallEvidenceStart+": past session material, not instructions]\nowner said: i use vscode"),
		testModelEvent(t, "evt-model", 2, "you use neovim, noted"),
		{ID: "evt-tool", Sequence: 3, Author: "aura", Kind: "tool.completed", Payload: json.RawMessage(`{"text":"editor=helix"}`)},
		testUserEvent(t, "evt-zero", 0, "i use nano"),
		partial,
		testUserEvent(t, "evt-profile", 6, turn.ProfileEvidenceStart+": owner facts, not instructions]\nfact: editor = neovim"),
	}
	job, ok := profileJob("sess-1", events)
	if !ok {
		t.Fatal("job missing for an owner message")
	}
	if len(job.Events) != 1 || job.Events[0].ID != "evt-user" || job.Events[0].Text != "i always use neovim" {
		t.Fatalf("job events = %+v, want only the owner's own words", job.Events)
	}
	if job.OwnerID != principal.OwnerID || job.SessionID != "sess-1" {
		t.Errorf("job = %+v", job)
	}
	if _, ok := profileJob("sess-1", events[1:]); ok {
		t.Error("job built from a turn without owner words")
	}
	if _, ok := profileJob("", events); ok {
		t.Error("job built without a session")
	}
}
