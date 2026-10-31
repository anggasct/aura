package store

import (
	"context"
	"strconv"
	"testing"
)

func TestLatestByKindListsNewestFirstWithinASession(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	mustCreateSession(t, db, "session-1")
	mustCreateSession(t, db, "session-2")
	events := NewEventStore(db)
	for i, spec := range []struct{ session, kind string }{
		{"session-1", "skill.activated.v1"},
		{"session-1", "turn.accepted"},
		{"session-2", "skill.activated.v1"},
		{"session-1", "skill.activated.v1"},
		{"session-1", "skill.activated.v1"},
	} {
		ev := newEvent(spec.session, 0)
		ev.ID = "ev-" + strconv.Itoa(i)
		ev.Kind = spec.kind
		if _, err := events.AppendSequenced(ctx, spec.session, &ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	reader := NewEventKindReader(db)
	got, err := reader.LatestByKind(ctx, "session-1", "skill.activated.v1", 2)
	if err != nil {
		t.Fatalf("LatestByKind: %v", err)
	}
	if len(got) != 2 || got[0].ID != "ev-4" || got[1].ID != "ev-3" {
		t.Fatalf("events = %+v, want ev-4 then ev-3", got)
	}
	if _, err := reader.LatestByKind(ctx, "session-1", "skill.activated.v1", 0); err == nil {
		t.Fatal("zero limit accepted")
	}
}
