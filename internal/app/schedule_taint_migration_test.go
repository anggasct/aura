package app

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/platform/sqlite"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const (
	legacyJobSession = "chat-session"
	legacyTaintLabel = "content this conversation held before taint tracking"
)

type legacyJob struct {
	id       string
	workflow string
}

func seedLegacyJob(t *testing.T, db *sql.DB, job legacyJob) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO scheduled_job
		(id, name, cron_expression, timezone, prompt, origin_channel, origin_destination, overlap_policy, catch_up_grace_seconds, state, version, created_at, updated_at, workflow_id)
		VALUES (?1, ?1, '0 21 * * *', 'UTC', CASE WHEN ?2 = '' THEN 'summarize' ELSE '' END, 'discord', 'default', 'skip', 900, 'active', 1, '2026-09-12T12:00:00Z', '2026-09-12T12:00:00Z', NULLIF(?2, ''))`,
		job.id, job.workflow); err != nil {
		t.Fatalf("seed job %s: %v", job.id, err)
	}
}

func seedReceipt(t *testing.T, db *sql.DB, key, operationID, surface, result string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO operation_receipt (idempotency_key, operation_id, request_digest, surface, result_json, created_at)
		VALUES (?, ?, 'digest', ?, ?, '2026-09-12T12:00:00Z')`, key, operationID, surface, result); err != nil {
		t.Fatalf("seed receipt %s: %v", key, err)
	}
}

func seedFunctionResponse(t *testing.T, db *sql.DB, sequence int, name string, response map[string]any) {
	t.Helper()
	event := &session.Event{
		ID: "event-" + name + "-" + string(rune('a'+sequence)), Author: "user", Timestamp: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
		LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call", Name: name, Response: response}}}}},
	}
	runtimeEvent, err := turnstore.RuntimeEventFromADK(legacyJobSession, "turn-1", event)
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, branch, author, kind, schema_version, payload_json, created_at)
		VALUES (?, ?, ?, 'turn-1', '', '', 'user', ?, 1, ?, '2026-09-12T12:00:00Z')`,
		runtimeEvent.ID, legacyJobSession, sequence, runtimeEvent.Kind, string(runtimeEvent.Payload)); err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

type markedJob struct {
	tainted bool
	source  sql.NullString
	context string
}

func readMarkedJobs(t *testing.T, db *sql.DB) map[string]markedJob {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT id, tainted, taint_source, context FROM scheduled_job`)
	if err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	jobs := map[string]markedJob{}
	for rows.Next() {
		var id string
		var job markedJob
		if err := rows.Scan(&id, &job.tainted, &job.source, &job.context); err != nil {
			t.Fatalf("scan job: %v", err)
		}
		jobs[id] = job
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read jobs rows: %v", err)
	}
	return jobs
}

func TestAgentCreatedJobsAreTaintedOnUpgrade(t *testing.T) {
	ctx := t.Context()
	history := Migrations()
	step := slices.IndexFunc(history, func(m sqlite.Migration) bool { return m.Owner == "schedule" && m.Version == 29 })
	if step < 0 {
		t.Fatal("the schedule context step is not in the history")
	}
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "legacy.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, history[:step]); err != nil {
		t.Fatalf("Migrate before the step: %v", err)
	}
	seedPersonaSession(t, db, legacyJobSession, "local-owner", "main")
	for _, job := range []legacyJob{
		{id: "from-receipt"}, {id: "from-event"}, {id: "from-both"}, {id: "workflow-from-receipt", workflow: "routine_weekly"},
		{id: "by-owner-chat"}, {id: "by-owner-cli"}, {id: "failed-add"}, {id: "other-tool-reply"},
	} {
		seedLegacyJob(t, db, job)
	}
	seedReceipt(t, db, "r1", "cron.add", "agent", `{"id":"from-receipt"}`)
	seedReceipt(t, db, "r2", "cron.add", "agent", `{"id":"from-both"}`)
	seedReceipt(t, db, "r3", "cron.add", "agent", `{"id":"workflow-from-receipt"}`)
	seedReceipt(t, db, "r4", "cron.add", "chat", `{"id":"by-owner-chat"}`)
	seedReceipt(t, db, "r5", "cron.pause", "agent", `{"id":"by-owner-cli"}`)
	seedFunctionResponse(t, db, 1, "aura_cron_add", map[string]any{"id": "from-event"})
	seedFunctionResponse(t, db, 2, "aura_cron_add", map[string]any{"id": "from-both"})
	seedFunctionResponse(t, db, 3, "aura_cron_add", map[string]any{"error": "the preview is stale", "id": "failed-add"})
	seedFunctionResponse(t, db, 4, "aura_cron_pause", map[string]any{"id": "other-tool-reply"})
	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_job`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if err := sqlite.Migrate(ctx, db, history); err != nil {
		t.Fatalf("Migrate the step: %v", err)
	}

	got := readMarkedJobs(t, db)
	if len(got) != before {
		t.Fatalf("jobs after the step = %d, want %d", len(got), before)
	}
	tainted := []string{"from-receipt", "from-event", "from-both", "workflow-from-receipt"}
	for id, job := range got {
		wantTainted := slices.Contains(tainted, id)
		if job.tainted != wantTainted || job.context != schedule.ContextFull {
			t.Errorf("%s = %+v, want tainted=%t context=full", id, job, wantTainted)
		}
		if wantTainted && job.source.String != legacyTaintLabel {
			t.Errorf("%s taint source = %q, want %q", id, job.source.String, legacyTaintLabel)
		}
		if !wantTainted && job.source.Valid {
			t.Errorf("%s carries a taint source %q without a mark", id, job.source.String)
		}
	}
}
