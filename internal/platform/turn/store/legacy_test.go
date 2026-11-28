package turnstore

import (
	"encoding/json"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
)

func testToolMayTaint(name string) bool {
	switch name {
	case "clean_tool":
		return false
	case "web_fetch", "web_search":
		return true
	default:
		return true
	}
}

func TestLegacySessionsAreMarkedOnce(t *testing.T) {
	ctx := t.Context()
	db := sqlitetest.OpenMigrated(t, Schema())
	sessions := NewSessionService(db)
	events := NewEventStore(db)
	now := time.Now().UTC()

	for _, id := range []string{"legacy-clean", "legacy-fetched", "legacy-already-marked"} {
		if err := sessions.Create(ctx, &turn.Session{ID: id, OwnerID: principal.OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	acceptedPayload, err := turn.AcceptedPayload(string(turn.OriginTerminal), principal.OwnerID, "main", nil)
	if err != nil {
		t.Fatalf("AcceptedPayload: %v", err)
	}
	for _, id := range []string{"legacy-clean", "legacy-fetched", "legacy-already-marked"} {
		if _, err := events.AppendSequenced(ctx, id, &turn.Event{
			ID: id + "-acc", SessionID: id, TurnID: "turn-1", Author: principal.OwnerID,
			Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: acceptedPayload, CreatedAt: now,
		}); err != nil {
			t.Fatalf("append accepted(%s): %v", id, err)
		}
	}

	userMsg, err := RuntimeEventFromADK("legacy-clean", "turn-1", &session.Event{
		ID: "ev-clean-1", Author: "user", Timestamp: now,
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}},
	})
	if err != nil {
		t.Fatalf("RuntimeEventFromADK clean: %v", err)
	}
	if _, err := events.AppendSequenced(ctx, "legacy-clean", &userMsg); err != nil {
		t.Fatalf("append clean: %v", err)
	}
	cleanResp, err := RuntimeEventFromADK("legacy-clean", "turn-1", &session.Event{
		ID: "ev-clean-tool", Author: "aura", Timestamp: now,
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{
			Role: "user",
			Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID:       "call-clean",
				Name:     "clean_tool",
				Response: map[string]any{"ok": true},
			}}},
		}},
	})
	if err != nil {
		t.Fatalf("RuntimeEventFromADK clean tool: %v", err)
	}
	if _, err := events.AppendSequenced(ctx, "legacy-clean", &cleanResp); err != nil {
		t.Fatalf("append clean tool: %v", err)
	}

	fetchResp, err := RuntimeEventFromADK("legacy-fetched", "turn-1", &session.Event{
		ID: "ev-fetch-1", Author: "aura", Timestamp: now,
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{
			Role: "user",
			Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID:       "call-1",
				Name:     "web_fetch",
				Response: map[string]any{"body": "external page"},
			}}},
		}},
	})
	if err != nil {
		t.Fatalf("RuntimeEventFromADK fetch: %v", err)
	}
	fetchSeq, err := events.AppendSequenced(ctx, "legacy-fetched", &fetchResp)
	if err != nil {
		t.Fatalf("append fetch: %v", err)
	}

	existingPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_search"})
	if _, err := events.AppendSequenced(ctx, "legacy-already-marked", &turn.Event{
		ID: "existing-mark", SessionID: "legacy-already-marked", TurnID: "turn-1", Author: "system",
		Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: existingPayload, CreatedAt: now,
	}); err != nil {
		t.Fatalf("append existing mark: %v", err)
	}

	first, err := MarkLegacySessions(ctx, db, testToolMayTaint)
	if err != nil {
		t.Fatalf("first MarkLegacySessions: %v", err)
	}
	if len(first) != 1 || first[0].SessionID != "legacy-fetched" || first[0].AtSequence != fetchSeq {
		t.Fatalf("first marked = %+v, want [{legacy-fetched %d}]", first, fetchSeq)
	}

	reader := NewTaintReader(db)
	if _, tainted, err := reader.SessionTaint(ctx, "legacy-clean", "turn-2", principal.OwnerID); err != nil || tainted {
		t.Fatalf("legacy-clean tainted = %v %v, want clean", tainted, err)
	}
	mark, tainted, err := reader.SessionTaint(ctx, "legacy-fetched", "turn-2", principal.OwnerID)
	if err != nil || !tainted || mark.Source != turn.TaintSourceLegacy || mark.AtSequence != int64(fetchSeq) {
		t.Fatalf("legacy-fetched mark = %+v %v %v, want legacy at %d", mark, tainted, err, fetchSeq)
	}

	second, err := MarkLegacySessions(ctx, db, testToolMayTaint)
	if err != nil {
		t.Fatalf("second MarkLegacySessions: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second marked = %+v, want none (idempotent)", second)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runtime_event WHERE session_id = ? AND kind = ?`,
		"legacy-fetched", turn.EventKindSessionTainted,
	).Scan(&count); err != nil {
		t.Fatalf("count taint marks: %v", err)
	}
	if count != 1 {
		t.Fatalf("legacy-fetched mark count = %d, want 1", count)
	}
}

func TestLegacyDerivationFailsClosedOnAllUntrustedSources(t *testing.T) {
	httpAcc, _ := turn.AcceptedPayload(string(turn.OriginHTTP), principal.OwnerID, "main", nil)
	webhookAcc, _ := turn.AcceptedPayload("webhook:github-1", principal.OwnerID, "main", nil)
	unknownOriginAcc, _ := turn.AcceptedPayload("custom:weird", principal.OwnerID, "main", nil)
	otherPrincipalAcc, _ := turn.AcceptedPayload("telegram:bot", "telegram:999", "main", nil)

	cases := []struct {
		name      string
		event     turn.Event
		wantTaint bool
	}{
		{
			name:      "http origin taints",
			event:     turn.Event{Sequence: 1, Author: principal.OwnerID, Kind: turn.EventKindTurnAccepted, Payload: httpAcc},
			wantTaint: true,
		},
		{
			name:      "webhook channel origin taints",
			event:     turn.Event{Sequence: 1, Author: principal.OwnerID, Kind: turn.EventKindTurnAccepted, Payload: webhookAcc},
			wantTaint: true,
		},
		{
			name:      "unknown origin fails closed",
			event:     turn.Event{Sequence: 1, Author: principal.OwnerID, Kind: turn.EventKindTurnAccepted, Payload: unknownOriginAcc},
			wantTaint: true,
		},
		{
			name:      "non-owner principal taints",
			event:     turn.Event{Sequence: 1, Author: "telegram:999", Kind: turn.EventKindTurnAccepted, Payload: otherPrincipalAcc},
			wantTaint: true,
		},
		{
			name:      "inlineData part taints",
			event:     turn.Event{Sequence: 2, Kind: EventKindADK, Payload: json.RawMessage(`{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}`)},
			wantTaint: true,
		},
		{
			name:      "fileData part taints",
			event:     turn.Event{Sequence: 2, Kind: EventKindADK, Payload: json.RawMessage(`{"content":{"parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"file:///tmp/a.pdf"}}]}}`)},
			wantTaint: true,
		},
		{
			name:      "attachment reference line taints",
			event:     turn.Event{Sequence: 2, Kind: EventKindADK, Payload: json.RawMessage(`{"content":{"parts":[{"text":"see\n[attachment \"doc.txt\" text/plain 12 bytes artifact:ref-1]"}]}}`)},
			wantTaint: true,
		},
		{
			name:      "corrupt adk json fails closed",
			event:     turn.Event{Sequence: 2, Kind: EventKindADK, Payload: json.RawMessage(`{corrupt`)},
			wantTaint: true,
		},
		{
			name:      "tool.completed for tainting tool taints",
			event:     turn.Event{Sequence: 3, Kind: "tool.completed", Payload: json.RawMessage(`{"tool_name":"web_fetch","status":"ok"}`)},
			wantTaint: true,
		},
		{
			name:      "unknown tool name fails closed",
			event:     turn.Event{Sequence: 3, Kind: EventKindADK, Payload: json.RawMessage(`{"content":{"parts":[{"functionResponse":{"name":"removed_tool","response":{"ok":true}}}]}}`)},
			wantTaint: true,
		},
		{
			name:      "known clean tool stays clean",
			event:     turn.Event{Sequence: 3, Kind: "tool.completed", Payload: json.RawMessage(`{"tool_name":"clean_tool","status":"ok"}`)},
			wantTaint: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq, found := turn.FirstLegacyUntrustedSequence(principal.OwnerID, []turn.Event{tc.event}, testToolMayTaint)
			if found != tc.wantTaint {
				t.Fatalf("FirstLegacyUntrustedSequence() = (%d, %v), want tainted=%v", seq, found, tc.wantTaint)
			}
			if tc.wantTaint && seq != tc.event.Sequence {
				t.Fatalf("seq = %d, want %d", seq, tc.event.Sequence)
			}
		})
	}
}
