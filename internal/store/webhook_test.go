package store

import (
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

	want := webhookFixture(now)
	if err := s.InsertExecution(ctx, want); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}

	got, err := s.Execution(ctx, "whex_1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if got.KeyID != "primary" || got.EventID != "evt-1" || got.Nonce != "nonce-abcdefghijklmnop" ||
		got.BodyDigest != "digest-1" || got.TurnID != "turn_1" || got.State != WebhookExecutionStateAccepted {
		t.Errorf("identity mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Errorf("timestamp mismatch: %+v", got)
	}
	if got.ResultEventID != "" || got.ErrorCode != "" {
		t.Errorf("expected empty result fields, got %+v", got)
	}

	byNonce, found, err := s.ExecutionByNonce(ctx, "primary", "nonce-abcdefghijklmnop")
	if err != nil || !found || byNonce.ID != "whex_1" {
		t.Errorf("ExecutionByNonce = %+v, %v, %v; want whex_1, true, nil", byNonce, found, err)
	}
	byEvent, found, err := s.ExecutionByEvent(ctx, "primary", "evt-1")
	if err != nil || !found || byEvent.ID != "whex_1" {
		t.Errorf("ExecutionByEvent = %+v, %v, %v; want whex_1, true, nil", byEvent, found, err)
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
