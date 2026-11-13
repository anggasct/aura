package app

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimesessions "github.com/anggasct/aura/internal/platform/turn/sessions"
	"github.com/anggasct/aura/internal/store"
)

const (
	recoveredWebhookTurnID      = "turn-webhook-left"
	recoveredWebhookSessionID   = "sess-webhook-left"
	recoveredWebhookExecutionID = "whex_left"
	recoveredExecutionWait      = 30 * time.Second
	recoveredExecutionPoll      = 20 * time.Millisecond
)

func webhookServerYAML(t *testing.T) string {
	t.Helper()
	t.Setenv("AURA_TEST_WEBHOOK_SECRET", "webhook-secret-0123456789abcdef")
	return "webhook:\n  enabled: true\n  listen_address: " + freeLoopbackAddress(t) +
		"\n  keys:\n    - id: primary\n      secret_env: AURA_TEST_WEBHOOK_SECRET\n"
}

func TestCompositionServerRegistersTheWebhookReconcile(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, webhookServerYAML(t)))
	app := buildApp(t, serverCompositionOptions(result, &syncBuffer{}))
	requireNames(t, "entries", app.group.Names(), []string{
		"sqlite", "telemetry", "memory_purge", "broadcast_resume", "schedule_resume", "schedule_pass", "turn_recovery", "channels",
		"durable_host", "turn_engine", "turn_recovery_pass", "workflow_proposal_cards", "webhook_reconcile", "retention_sweeps", "run_retention", "workflow_completion", "persona_eval_schedules",
	})
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close without Run: %v", err)
	}
}

func admitLeftWebhookTurn(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{
		ID: recoveredWebhookSessionID, OwnerID: "webhook:primary", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create the session: %v", err)
	}
	if err := store.NewWebhookExecutionStore(db).InsertExecution(t.Context(), &store.WebhookExecution{
		ID: recoveredWebhookExecutionID, KeyID: "primary", EventID: "evt-left", Nonce: "nonce-left-0123456789", BodyDigest: "digest-left",
		TurnID: recoveredWebhookTurnID, SessionID: recoveredWebhookSessionID, State: store.WebhookExecutionStateAccepted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}
	sessions, err := sqlite.NewSessionStore(&sqlite.SessionStoreOptions{DB: db})
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	admitted, err := sessions.Admit(t.Context(), &runtimesessions.AdmitRequest{Turn: runtimesessions.Descriptor{
		TurnID: recoveredWebhookTurnID, SessionID: recoveredWebhookSessionID, PrincipalID: "webhook:primary",
		PrincipalClass: string(principal.ClassTrusted), Origin: string(turn.OriginWebhook),
		Parts: []runtimesessions.Part{{Text: "hello"}, {Text: `{"k":1}`}}, AgentID: "main",
		IdempotencyKey: "webhook:primary:evt-left",
	}, AcceptedEventID: "evt-webhook-left-accepted", MaxPending: 16})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if admitted.ToStart == nil {
		t.Fatal("the left webhook turn was queued, want granted")
	}
}

func awaitExecutionState(t *testing.T, db *sql.DB, want string) store.WebhookExecution {
	t.Helper()
	executions := store.NewWebhookExecutionStore(db)
	deadline := time.NewTimer(recoveredExecutionWait)
	defer deadline.Stop()
	tick := time.NewTicker(recoveredExecutionPoll)
	defer tick.Stop()
	for {
		execution, err := executions.Execution(t.Context(), recoveredWebhookExecutionID)
		if err != nil {
			t.Fatalf("Execution: %v", err)
		}
		if execution.State == want {
			return execution
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("execution = %s after %s, want %s", execution.State, recoveredExecutionWait, want)
		}
	}
}

func TestServerSettlesTheExecutionOfARecoveredWebhookTurn(t *testing.T) {
	restoreDefaultLogger(t)
	model := newStubModel(t, stubAnswers)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	extra := "runtime:\n  shutdown_timeout: " + serverTurnShutdownTimeout.String() + "\n" + webhookServerYAML(t)
	path := writeCompositionConfig(t, freeLoopbackAddress(t), stubModelsYAML(model.url), extra)
	seedConfiguredStorage(t, path)
	db := compositionDatabase(t, path)
	admitLeftWebhookTurn(t, db)
	srv := buildTurnServer(t, path)
	srv.run(t)
	execution := awaitExecutionState(t, db, store.WebhookExecutionStateCompleted)
	var completedEventID string
	if err := db.QueryRowContext(t.Context(), `SELECT id FROM runtime_event WHERE session_id = ? AND turn_id = ? AND kind = ?`,
		recoveredWebhookSessionID, recoveredWebhookTurnID, turn.EventKindTurnCompleted).Scan(&completedEventID); err != nil {
		t.Fatalf("read the completed event: %v", err)
	}
	if execution.ResultEventID != completedEventID {
		t.Fatalf("execution result = %q, want the completed turn event %q", execution.ResultEventID, completedEventID)
	}
	if got := model.calls.Load(); got != 1 {
		t.Fatalf("model calls = %d, want 1", got)
	}
	srv.close(t)
}
