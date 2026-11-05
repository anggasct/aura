package store

import (
	"database/sql"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func appendTextEvent(t *testing.T, db *sql.DB, sessionID, id, text string, at time.Time) {
	t.Helper()
	event := &session.Event{ID: id, InvocationID: id, Author: "lobby", Timestamp: at}
	event.Content = genai.NewContentFromText(text, genai.RoleModel)
	stored, err := RuntimeEventFromADK(sessionID, id, event)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK(%s): %v", id, err)
	}
	if _, err := NewEventStore(db).AppendSequenced(t.Context(), sessionID, &stored); err != nil {
		t.Fatalf("AppendSequenced(%s): %v", id, err)
	}
}

func TestHandoffReplyEventIDIsPrefixedByTheSharedConstant(t *testing.T) {
	at := time.Date(2026, 11, 5, 9, 0, 0, 5, time.UTC)
	if got, want := HandoffReplyEventID(at), HandoffReplyEventPrefix+"1793869200000000005"; got != want {
		t.Fatalf("HandoffReplyEventID = %q, want %q", got, want)
	}
}

func TestHandoffReplyStoreListsOnlyOwnerRepliesOfTheSession(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	mustCreateSession(t, db, "lobby/http/one")
	mustCreateSession(t, db, "lobby/http/two")
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	appendTextEvent(t, db, "lobby/http/one", "turn-1-answer", "persona answer", base)
	appendTextEvent(t, db, "lobby/http/one", HandoffReplyEventID(base.Add(time.Second)), "first reply", base.Add(time.Second))
	appendTextEvent(t, db, "lobby/http/two", HandoffReplyEventID(base.Add(2*time.Second)), "other session reply", base.Add(2*time.Second))
	appendTextEvent(t, db, "lobby/http/one", "turn-2-answer", "later persona answer", base.Add(3*time.Second))
	appendTextEvent(t, db, "lobby/http/one", HandoffReplyEventID(base.Add(4*time.Second)), "second reply", base.Add(4*time.Second))
	replies := NewHandoffReplyStore(db)

	tests := []struct {
		name  string
		after uint64
		limit int
		want  []HandoffReply
	}{
		{
			name: "all owner replies oldest first", limit: 10,
			want: []HandoffReply{
				{Sequence: 2, Text: "first reply", CreatedAt: base.Add(time.Second)},
				{Sequence: 4, Text: "second reply", CreatedAt: base.Add(4 * time.Second)},
			},
		},
		{
			name: "after skips seen replies", after: 2, limit: 10,
			want: []HandoffReply{{Sequence: 4, Text: "second reply", CreatedAt: base.Add(4 * time.Second)}},
		},
		{
			name: "limit is applied to replies not to events", limit: 1,
			want: []HandoffReply{{Sequence: 2, Text: "first reply", CreatedAt: base.Add(time.Second)}},
		},
		{name: "after the last reply", after: 4, limit: 10, want: []HandoffReply{}},
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
	replies := NewHandoffReplyStore(newTestDB(t))
	if _, err := replies.List(t.Context(), "lobby/http/one", 0, 0); !hasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("zero limit err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := replies.List(t.Context(), "lobby/http/one", ^uint64(0), 1); !hasCode(err, ErrorCodeEventSequenceInvalid) {
		t.Fatalf("oversized cursor err = %v, want %s", err, ErrorCodeEventSequenceInvalid)
	}
}

func hasCode(err error, want ErrorCode) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}
