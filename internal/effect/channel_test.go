package effect

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

func validChannelPrepare(key string) *PrepareRequest {
	return &PrepareRequest{
		SessionID:      "sess-1",
		IdempotencyKey: key,
		Provider:       "discord",
		Operation:      "send_message",
		Classification: ClassificationEffectful,
		Request:        json.RawMessage(`{"channel_id":"333","text":"hi"}`),
		EventKind:      EventKindChannelRequested,
	}
}

func channelSequences(t *testing.T, db *sql.DB) []uint64 {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT sequence FROM runtime_event WHERE kind = ? ORDER BY sequence ASC`, EventKindChannelRequested)
	if err != nil {
		t.Fatalf("query sequences: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var sequences []uint64
	for rows.Next() {
		var sequence uint64
		if err := rows.Scan(&sequence); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		sequences = append(sequences, sequence)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sequences: %v", err)
	}
	return sequences
}

func TestPrepare_ChannelAllocatesIncreasingSequences(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	first := mustPrepare(t, j, validChannelPrepare("chan-1"))
	second := mustPrepare(t, j, validChannelPrepare("chan-2"))

	if first.State != StatePrepared || second.State != StatePrepared {
		t.Fatalf("states = %s, %s; want prepared", first.State, second.State)
	}
	if got := eventCount(t, db, EventKindChannelRequested); got != 2 {
		t.Fatalf("channel.requested events = %d, want 2", got)
	}
	sequences := channelSequences(t, db)
	if len(sequences) != 2 || sequences[1] <= sequences[0] {
		t.Fatalf("sequences = %v, want strictly increasing", sequences)
	}
}

func TestPrepare_ChannelRejectsInvalid(t *testing.T) {
	t.Parallel()
	j, _ := newTestJournal(t)

	cases := map[string]func(*PrepareRequest){
		"turn id set": func(req *PrepareRequest) {
			req.TurnID = "turn-1"
		},
		"tool call id set": func(req *PrepareRequest) {
			req.ToolCallID = "call-1"
		},
		"sequence supplied": func(req *PrepareRequest) {
			req.EventSequence = 9
		},
		"empty session": func(req *PrepareRequest) {
			req.SessionID = ""
		},
		"empty key": func(req *PrepareRequest) {
			req.IdempotencyKey = ""
		},
		"bad classification": func(req *PrepareRequest) {
			req.Classification = "whatever"
		},
		"bad request json": func(req *PrepareRequest) {
			req.Request = json.RawMessage(`{oops`)
		},
		"unknown kind": func(req *PrepareRequest) {
			req.EventKind = "carrier.pigeon"
			req.TurnID = "turn-1"
			req.ToolCallID = "call-1"
			req.EventSequence = 1
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := validChannelPrepare("chan-" + name)
			mutate(req)
			_, err := j.Prepare(context.Background(), req)
			if name == "bad classification" {
				assertCode(t, err, ErrorCodeClassificationMissing)
				return
			}
			assertCode(t, err, ErrorCodeInvalidArgument)
		})
	}
}

func TestPrepare_ChannelReplayAndConflict(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)

	first := mustPrepare(t, j, validChannelPrepare("chan-1"))
	replay := validChannelPrepare("chan-1")
	replay.EventID = "other-event"
	second, err := j.Prepare(context.Background(), replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned %s, want %s", second.ID, first.ID)
	}
	if got := eventCount(t, db, EventKindChannelRequested); got != 1 {
		t.Fatalf("events after replay = %d, want 1", got)
	}

	clash := validChannelPrepare("chan-1")
	clash.Request = json.RawMessage(`{"channel_id":"333","text":"other"}`)
	if _, err := j.Prepare(context.Background(), clash); err == nil {
		t.Fatal("different digest accepted on existing key")
	} else {
		assertCode(t, err, ErrorCodeIdempotencyConflict)
	}
}

func TestPrepare_ChannelPublishesLiveEvent(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	published := make(chan *store.RuntimeEvent, 1)
	j.SetEventPublisher(&channelPublisher{events: published})

	req := validChannelPrepare("chan-live")
	req.EventID = "evt-chan-live"
	if _, err := j.Prepare(context.Background(), req); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	var live *store.RuntimeEvent
	select {
	case live = <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("channel.requested did not reach the subscriber")
	}
	if live.Kind != EventKindChannelRequested {
		t.Fatalf("published kind = %q, want %q", live.Kind, EventKindChannelRequested)
	}
	if live.SchemaVersion != channelRequestedSchemaVersion {
		t.Fatalf("published schema = %d, want %d", live.SchemaVersion, channelRequestedSchemaVersion)
	}
	if live.Sequence == 0 {
		t.Fatal("published sequence is zero, want allocated sequence")
	}
	if live.ID != req.EventID || live.SessionID != req.SessionID {
		t.Fatalf("published identity = %+v, want id %q session %q", live, req.EventID, req.SessionID)
	}
	var (
		kind    string
		seq     uint64
		version uint16
		payload string
	)
	if err := db.QueryRowContext(context.Background(),
		`SELECT kind, sequence, schema_version, payload_json FROM runtime_event WHERE id = ?`, req.EventID).Scan(&kind, &seq, &version, &payload); err != nil {
		t.Fatalf("query persisted event: %v", err)
	}
	if live.Sequence != seq || live.Kind != kind || live.SchemaVersion != version {
		t.Fatalf("live %+v diverges from persisted kind %q seq %d schema %d", live, kind, seq, version)
	}
	if string(live.Payload) != payload {
		t.Fatal("live payload diverges from persisted payload")
	}
}
