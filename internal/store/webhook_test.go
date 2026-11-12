package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func webhookFixture(now time.Time) *WebhookExecution {
	return &WebhookExecution{
		ID:         "whex_1",
		KeyID:      "primary",
		EventID:    "evt-1",
		Nonce:      "nonce-abcdefghijklmnop",
		BodyDigest: "digest-1",
		TurnID:     "turn_1",
		SessionID:  "sess_1",
		State:      WebhookExecutionStateAccepted,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
}

func TestWebhookExecutionStore_InsertAndRead(t *testing.T) {
	db := newTestDB(t)
	s := NewWebhookExecutionStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	withoutSession := webhookFixture(now)
	withoutSession.SessionID = ""
	if code, ok := CodeOf(s.InsertExecution(ctx, withoutSession)); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("insert without a session = %v (ok=%v), want %s", code, ok, ErrorCodeInvalidArgument)
	}

	want := webhookFixture(now)
	if err := s.InsertExecution(ctx, want); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}

	got, err := s.Execution(ctx, "whex_1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if got.KeyID != "primary" || got.EventID != "evt-1" || got.Nonce != "nonce-abcdefghijklmnop" ||
		got.BodyDigest != "digest-1" || got.TurnID != "turn_1" || got.SessionID != "sess_1" || got.State != WebhookExecutionStateAccepted {
		t.Errorf("identity mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Errorf("timestamp mismatch: %+v", got)
	}
	if got.ResultEventID != "" || got.ErrorCode != "" {
		t.Errorf("expected empty result fields, got %+v", got)
	}

	byNonce, found, err := s.ExecutionByNonce(ctx, "primary", "nonce-abcdefghijklmnop")
	if err != nil || !found || byNonce.ID != "whex_1" || byNonce.SessionID != "sess_1" {
		t.Errorf("ExecutionByNonce = %+v, %v, %v; want whex_1 in sess_1, true, nil", byNonce, found, err)
	}
	byEvent, found, err := s.ExecutionByEvent(ctx, "primary", "evt-1")
	if err != nil || !found || byEvent.ID != "whex_1" || byEvent.SessionID != "sess_1" {
		t.Errorf("ExecutionByEvent = %+v, %v, %v; want whex_1 in sess_1, true, nil", byEvent, found, err)
	}
	if _, found, err := s.ExecutionByNonce(ctx, "primary", "nonce-missing"); err != nil || found {
		t.Errorf("ExecutionByNonce missing = %v, %v; want false, nil", found, err)
	}
}

func TestWebhookExecutionStore_Conflicts(t *testing.T) {
	db := newTestDB(t)
	s := NewWebhookExecutionStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := s.InsertExecution(ctx, webhookFixture(now)); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}

	duplicateNonce := webhookFixture(now)
	duplicateNonce.ID = "whex_2"
	duplicateNonce.EventID = "evt-2"
	duplicateNonce.TurnID = "turn_2"
	if err := s.InsertExecution(ctx, duplicateNonce); err == nil {
		t.Fatal("expected conflict on duplicate nonce, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeWebhookExecutionConflict {
		t.Fatalf("conflict code = %v (ok=%v), want %s", code, ok, ErrorCodeWebhookExecutionConflict)
	}

	duplicateEvent := webhookFixture(now)
	duplicateEvent.ID = "whex_3"
	duplicateEvent.Nonce = "nonce-other-12345678"
	duplicateEvent.TurnID = "turn_3"
	if err := s.InsertExecution(ctx, duplicateEvent); err == nil {
		t.Fatal("expected conflict on duplicate event, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeWebhookExecutionConflict {
		t.Fatalf("conflict code = %v (ok=%v), want %s", code, ok, ErrorCodeWebhookExecutionConflict)
	}

	if _, err := s.Execution(ctx, "whex-missing"); err == nil {
		t.Fatal("expected not found, got nil")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeWebhookExecutionNotFound {
		t.Fatalf("not-found code = %v (ok=%v), want %s", code, ok, ErrorCodeWebhookExecutionNotFound)
	}
}

func TestWebhookExecutionStore_ForwardOnlyTransitions(t *testing.T) {
	db := newTestDB(t)
	s := NewWebhookExecutionStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if err := s.InsertExecution(ctx, webhookFixture(now)); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?, ?, '{}', ?, ?)`,
		"sess_1", "webhook:primary", formatTime(now), formatTime(now)); err != nil {
		t.Fatalf("insert session fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, branch, author, kind, schema_version, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"evt_r1", "sess_1", 1, "turn_1", "inv_1", "", "webhook:primary", "message.completed", 1, `{}`, formatTime(now)); err != nil {
		t.Fatalf("insert event fixture: %v", err)
	}

	advanced, err := s.MarkRunning(ctx, "whex_1", now.Add(time.Minute))
	if err != nil || !advanced {
		t.Fatalf("accepted→running = %v, %v; want true, nil", advanced, err)
	}
	advanced, err = s.MarkRunning(ctx, "whex_1", now.Add(2*time.Minute))
	if err != nil || advanced {
		t.Fatalf("running→running = %v, %v; want false, nil", advanced, err)
	}
	advanced, err = s.MarkTerminal(ctx, "whex_1", WebhookExecutionStateCompleted, "evt_r1", "", now.Add(3*time.Minute))
	if err != nil || !advanced {
		t.Fatalf("running→completed = %v, %v; want true, nil", advanced, err)
	}
	advanced, err = s.MarkTerminal(ctx, "whex_1", WebhookExecutionStateFailed, "", "runtime_internal", now.Add(4*time.Minute))
	if err != nil || advanced {
		t.Fatalf("completed→failed = %v, %v; want false, nil", advanced, err)
	}
	if _, err := s.MarkTerminal(ctx, "whex_1", WebhookExecutionStateRunning, "", "", now.Add(5*time.Minute)); err == nil {
		t.Fatal("expected invalid terminal state to fail, got nil")
	}

	got, err := s.Execution(ctx, "whex_1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if got.State != WebhookExecutionStateCompleted || got.ResultEventID != "evt_r1" || got.ErrorCode != "" {
		t.Errorf("terminal row = %+v; want completed/evt_r1/empty", got)
	}
	if !got.UpdatedAt.Equal(now.Add(3 * time.Minute)) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, now.Add(3*time.Minute))
	}

	if err := s.DeleteExecution(ctx, "whex_1"); err != nil {
		t.Fatalf("DeleteExecution: %v", err)
	}
	if _, err := s.Execution(ctx, "whex_1"); err == nil {
		t.Fatal("expected not found after delete, got nil")
	}
}

func TestWebhookExecutionStore_ListsUnfinished(t *testing.T) {
	db := newTestDB(t)
	s := NewWebhookExecutionStore(db)
	ctx := t.Context()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		id, state string
		expiresAt time.Time
	}{
		{"whex_a", WebhookExecutionStateAccepted, now.Add(time.Hour)},
		{"whex_b", WebhookExecutionStateCompleted, now.Add(time.Hour)},
		{"whex_c", WebhookExecutionStateRunning, now.Add(time.Hour)},
		{"whex_d", WebhookExecutionStateFailed, now.Add(time.Hour)},
		{"whex_e", WebhookExecutionStateCancelled, now.Add(time.Hour)},
		{"whex_f", WebhookExecutionStateAccepted, now},
		{"whex_g", WebhookExecutionStateRunning, now.Add(2 * time.Hour)},
	}
	for _, row := range rows {
		execution := webhookFixture(now)
		execution.ID, execution.State, execution.ExpiresAt = row.id, row.state, row.expiresAt
		execution.EventID, execution.Nonce, execution.TurnID, execution.SessionID = "evt-"+row.id, "nonce-"+row.id, "turn-"+row.id, "sess-"+row.id
		if err := s.InsertExecution(ctx, execution); err != nil {
			t.Fatalf("InsertExecution %s: %v", row.id, err)
		}
	}
	listed := func(query *UnfinishedExecutionQuery) []string {
		t.Helper()
		executions, err := s.ListUnfinished(ctx, query)
		if err != nil {
			t.Fatalf("ListUnfinished: %v", err)
		}
		ids := make([]string, 0, len(executions))
		for _, execution := range executions {
			if execution.SessionID != "sess-"+execution.ID {
				t.Fatalf("listed execution %s has session %q", execution.ID, execution.SessionID)
			}
			ids = append(ids, execution.ID)
		}
		return ids
	}
	if got := listed(&UnfinishedExecutionQuery{Now: now, Limit: 10}); !slices.Equal(got, []string{"whex_a", "whex_c", "whex_g"}) {
		t.Fatalf("unfinished = %v, want the unexpired accepted and running rows in id order", got)
	}
	if got := listed(&UnfinishedExecutionQuery{Now: now, AfterID: "whex_a", Limit: 1}); !slices.Equal(got, []string{"whex_c"}) {
		t.Fatalf("one after whex_a = %v, want whex_c", got)
	}
	if _, err := s.ListUnfinished(ctx, &UnfinishedExecutionQuery{Now: now}); err == nil {
		t.Fatal("a list without a limit succeeded")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("list without a limit = %v (ok=%v), want %s", code, ok, ErrorCodeInvalidArgument)
	}
}

type seededExecution struct {
	id, state string
}

func seedExecutionAtVersion42(t *testing.T, db *sql.DB, execution seededExecution) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO webhook_execution (id, key_id, event_id, nonce, body_digest, turn_id, state, created_at, updated_at, expires_at)
		 VALUES (?, 'primary', ?, ?, 'digest', ?, ?, '2026-09-07T12:00:00Z', '2026-09-07T12:00:00Z', '2030-01-01T00:00:00Z')`,
		execution.id, "evt-"+execution.id, "nonce-"+execution.id, "turn-"+execution.id, execution.state); err != nil {
		t.Fatalf("seed execution %s: %v", execution.id, err)
	}
}

type seededTurnEvent struct {
	sessionID, turnID, kind string
}

func seedTurnEventAtVersion42(t *testing.T, db *sql.DB, event seededTurnEvent) {
	t.Helper()
	seedMemorySession(t, db, event.sessionID, "webhook:primary")
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, branch, author, kind, schema_version, payload_json, created_at)
		 VALUES (?, ?, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM runtime_event WHERE session_id = ?), ?, 'inv', '', 'webhook:primary', ?, 1, '{}', '2026-09-07T12:00:00Z')`,
		event.sessionID+"-"+event.kind, event.sessionID, event.sessionID, event.turnID, event.kind); err != nil {
		t.Fatalf("seed event %s: %v", event.kind, err)
	}
}

func TestWebhookExecutionSessionMigration(t *testing.T) {
	ctx := t.Context()
	db := openDatabaseAtVersion(t, filepath.Join(t.TempDir(), "aura.db"), 42)
	seedExecutionAtVersion42(t, db, seededExecution{id: "whex_completed", state: WebhookExecutionStateCompleted})
	seedTurnEventAtVersion42(t, db, seededTurnEvent{sessionID: "sess-other", turnID: "turn-other", kind: "turn.accepted"})
	seedTurnEventAtVersion42(t, db, seededTurnEvent{sessionID: "sess-completed", turnID: "turn-whex_completed", kind: "turn.accepted"})
	seedTurnEventAtVersion42(t, db, seededTurnEvent{sessionID: "sess-completed", turnID: "turn-whex_completed", kind: "turn.completed"})
	seedExecutionAtVersion42(t, db, seededExecution{id: "whex_running", state: WebhookExecutionStateRunning})
	seedTurnEventAtVersion42(t, db, seededTurnEvent{sessionID: "sess-running", turnID: "turn-whex_running", kind: "turn.accepted"})
	seedExecutionAtVersion42(t, db, seededExecution{id: "whex_never_admitted", state: WebhookExecutionStateAccepted})
	seedExecutionAtVersion42(t, db, seededExecution{id: "whex_events_gone", state: WebhookExecutionStateCompleted})

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewWebhookExecutionStore(db)
	for id, want := range map[string]string{"whex_completed": "sess-completed", "whex_running": "sess-running", "whex_events_gone": ""} {
		got, err := s.Execution(ctx, id)
		if err != nil || got.SessionID != want {
			t.Errorf("%s = session %q, %v; want %q", id, got.SessionID, err, want)
		}
	}
	if _, err := s.Execution(ctx, "whex_never_admitted"); err == nil {
		t.Error("an unfinished execution without an admitted turn survived the migration")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeWebhookExecutionNotFound {
		t.Errorf("never admitted execution = %v (ok=%v), want %s", code, ok, ErrorCodeWebhookExecutionNotFound)
	}
	var index string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'webhook_execution' AND name = 'webhook_execution_unfinished_idx'`).Scan(&index); err != nil {
		t.Fatalf("index webhook_execution_unfinished_idx: %v", err)
	}
}
