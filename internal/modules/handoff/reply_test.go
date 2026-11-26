package handoff

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func mustCreateSession(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	sess := &turn.Session{
		ID:        id,
		OwnerID:   "owner-1",
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  json.RawMessage(`{}`),
	}
	if err := turnstore.NewSessionService(db).Create(t.Context(), sess); err != nil {
		t.Fatalf("Create session(%s): %v", id, err)
	}
}

func appendTextEvent(t *testing.T, db *sql.DB, sessionID, id, text string, at time.Time) {
	t.Helper()
	event := &session.Event{ID: id, InvocationID: id, Author: "lobby", Timestamp: at}
	event.Content = genai.NewContentFromText(text, genai.RoleModel)
	stored, err := turnstore.RuntimeEventFromADK(sessionID, id, event)
	if err != nil {
		t.Fatalf("turnstore.RuntimeEventFromADK(%s): %v", id, err)
	}
	if _, err := turnstore.NewEventStore(db).AppendSequenced(t.Context(), sessionID, &stored); err != nil {
		t.Fatalf("AppendSequenced(%s): %v", id, err)
	}
}

func mustNewReplyStore(t *testing.T, db *sql.DB) *SQLiteReplyStore {
	t.Helper()
	s, err := NewReplyStore(turnstore.NewTurnStore(db))
	if err != nil {
		t.Fatalf("NewReplyStore: %v", err)
	}
	return s
}

func TestHandoffReplyEventIDIsPrefixedByTheSharedConstant(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 0, 0, 5, time.UTC)
	if got, want := ReplyEventID(at), ReplyEventPrefix+"1793869200000000005"; got != want {
		t.Fatalf("ReplyEventID = %q, want %q", got, want)
	}
}

func TestHandoffReplyStoreListsOnlyOwnerRepliesOfTheSession(t *testing.T) {
	db := storetest.OpenMigrated(t)
	ctx := t.Context()
	mustCreateSession(t, db, "lobby/http/one")
	mustCreateSession(t, db, "lobby/http/two")
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	appendTextEvent(t, db, "lobby/http/one", "turn-1-answer", "persona answer", base)
	appendTextEvent(t, db, "lobby/http/one", ReplyEventID(base.Add(time.Second)), "first reply", base.Add(time.Second))
	appendTextEvent(t, db, "lobby/http/two", ReplyEventID(base.Add(2*time.Second)), "other session reply", base.Add(2*time.Second))
	appendTextEvent(t, db, "lobby/http/one", "turn-2-answer", "later persona answer", base.Add(3*time.Second))
	appendTextEvent(t, db, "lobby/http/one", ReplyEventID(base.Add(4*time.Second)), "second reply", base.Add(4*time.Second))
	replies := mustNewReplyStore(t, db)

	tests := []struct {
		name  string
		after uint64
		limit int
		want  []Reply
	}{
		{
			name: "all owner replies oldest first", limit: 10,
			want: []Reply{
				{Sequence: 2, Text: "first reply", CreatedAt: base.Add(time.Second)},
				{Sequence: 4, Text: "second reply", CreatedAt: base.Add(4 * time.Second)},
			},
		},
		{
			name: "after skips seen replies", after: 2, limit: 10,
			want: []Reply{{Sequence: 4, Text: "second reply", CreatedAt: base.Add(4 * time.Second)}},
		},
		{
			name: "limit is applied to replies not to events", limit: 1,
			want: []Reply{{Sequence: 2, Text: "first reply", CreatedAt: base.Add(time.Second)}},
		},
		{name: "after the last reply", after: 4, limit: 10, want: []Reply{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replies.List(ctx, "lobby/http/one", tt.after, tt.limit)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("List = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i].Sequence != tt.want[i].Sequence || got[i].Text != tt.want[i].Text || !got[i].CreatedAt.Equal(tt.want[i].CreatedAt) {
					t.Fatalf("List[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHandoffReplyStoreRejectsInvalidBounds(t *testing.T) {
	if _, err := NewReplyStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewReplyStore(nil) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	replies := mustNewReplyStore(t, storetest.OpenMigrated(t))
	if _, err := replies.List(t.Context(), "lobby/http/one", 0, 0); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("zero limit err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := replies.List(t.Context(), "lobby/http/one", ^uint64(0), 1); !fault.HasCode(err, turnstore.ErrorCodeEventSequenceInvalid) {
		t.Fatalf("oversized cursor err = %v, want %s", err, turnstore.ErrorCodeEventSequenceInvalid)
	}
}
