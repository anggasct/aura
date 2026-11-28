package turnstore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
)

func appendTaintMarkEvent(t *testing.T, events EventStore, sessionID, turnID, id string, payload json.RawMessage) uint64 {
	t.Helper()
	seq, err := events.AppendSequenced(t.Context(), sessionID, &turn.Event{
		ID:            id,
		SessionID:     sessionID,
		TurnID:        turnID,
		Author:        "system",
		Kind:          turn.EventKindSessionTainted,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendSequenced(%s): %v", id, err)
	}
	return seq
}

func TestSessionTaintLookup(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, Schema())
	sessions := NewSessionService(db)
	events := NewEventStore(db)
	reader := NewTaintReader(db)

	now := time.Now().UTC()
	for _, id := range []string{"sess-clean", "sess-tool", "sess-other", "sess-legacy", "sess-corrupt"} {
		if err := sessions.Create(ctx, &turn.Session{ID: id, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	if mark, tainted, err := reader.SessionTaint(ctx, "sess-clean", "turn-1", "owner-1"); err != nil || tainted || mark != (turn.TaintMark{}) {
		t.Fatalf("clean session = %+v %v %v, want clean", mark, tainted, err)
	}

	pTool, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
	toolSeq := appendTaintMarkEvent(t, events, "sess-tool", "turn-1", "ev-tool-1", pTool)

	if mark, tainted, err := reader.SessionTaint(ctx, "sess-tool", "turn-1", "owner-1"); err != nil || tainted || mark != (turn.TaintMark{}) {
		t.Fatalf("same turn lookup = %+v %v %v, want clean (current turn excluded)", mark, tainted, err)
	}
	mark, tainted, err := reader.SessionTaint(ctx, "sess-tool", "turn-2", "owner-1")
	if err != nil || !tainted || mark.Source != turn.TaintSourceToolResult || mark.Detail != "web_fetch" {
		t.Fatalf("later turn lookup = %+v %v %v, want tool_result web_fetch", mark, tainted, err)
	}

	pOther, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceOtherPrincipal, Principal: "guest-1"})
	appendTaintMarkEvent(t, events, "sess-other", "turn-g1", "ev-other-1", pOther)
	if mark, tainted, err := reader.SessionTaint(ctx, "sess-other", "turn-g2", "guest-1"); err != nil || tainted || mark != (turn.TaintMark{}) {
		t.Fatalf("other_principal for same principal = %+v %v %v, want clean", mark, tainted, err)
	}
	if mark, tainted, err := reader.SessionTaint(ctx, "sess-other", "turn-o1", "owner-1"); err != nil || !tainted || mark.Source != turn.TaintSourceOtherPrincipal {
		t.Fatalf("other_principal for owner = %+v %v %v, want other_principal", mark, tainted, err)
	}

	if _, err := events.AppendSequenced(ctx, "sess-legacy", &turn.Event{
		ID: "ev-leg-0", SessionID: "sess-legacy", TurnID: "turn-0", Author: "owner-1",
		Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{"origin":"terminal","principal":"owner-1"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("append ev-leg-0: %v", err)
	}
	pSecond, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_search"})
	appendTaintMarkEvent(t, events, "sess-legacy", "turn-2", "ev-leg-2", pSecond)
	pLegacy, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceLegacy, AtSequence: 1})
	appendTaintMarkEvent(t, events, "sess-legacy", "turn-legacy", "ev-leg-1", pLegacy)

	mark, tainted, err = reader.SessionTaint(ctx, "sess-legacy", "turn-3", "owner-1")
	if err != nil || !tainted || mark.Source != turn.TaintSourceLegacy {
		t.Fatalf("effective sequence precedence = %+v %v %v, want legacy at sequence 1", mark, tainted, err)
	}

	appendTaintMarkEvent(t, events, "sess-corrupt", "turn-1", "ev-corrupt-1", json.RawMessage(`{"bad":"mark"}`))
	mark, tainted, err = reader.SessionTaint(ctx, "sess-corrupt", "turn-2", "owner-1")
	if err != nil || !tainted || mark.Source != turn.TaintSourceLegacy {
		t.Fatalf("corrupt mark = %+v %v %v, want fail-closed legacy", mark, tainted, err)
	}

	marks, err := reader.SessionMarks(ctx, []string{"sess-clean", "sess-tool", "sess-legacy", "sess-corrupt"})
	if err != nil {
		t.Fatalf("SessionMarks: %v", err)
	}
	if _, ok := marks["sess-clean"]; ok {
		t.Errorf("clean session appeared in marks: %v", marks)
	}
	if marks["sess-tool"] != toolSeq {
		t.Errorf("sess-tool mark seq = %d, want %d", marks["sess-tool"], toolSeq)
	}
	if marks["sess-legacy"] != 1 {
		t.Errorf("sess-legacy mark seq = %d, want 1 (at_sequence)", marks["sess-legacy"])
	}
	if marks["sess-corrupt"] != 1 {
		t.Errorf("sess-corrupt mark seq = %d, want 1 (fail-closed)", marks["sess-corrupt"])
	}
}

func TestRuntimeEventSessionKindIndex(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, Schema())

	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'runtime_event_session_kind_idx'`,
	).Scan(&name); err != nil {
		t.Fatalf("runtime_event_session_kind_idx missing: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT sequence, turn_id, payload_json FROM runtime_event WHERE session_id = ? AND kind = ? ORDER BY sequence ASC`,
		"sess-1", turn.EventKindSessionTainted,
	)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	joined := strings.Join(details, " | ")
	if !strings.Contains(joined, "runtime_event_session_kind_idx") {
		t.Fatalf("query plan %q does not use runtime_event_session_kind_idx", joined)
	}
}
