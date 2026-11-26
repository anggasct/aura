package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const erasureWorkflowStamp = "2026-11-12T09:00:00Z"

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.OpenMigrated(t)
}

func erasureSeedMemorySession(t *testing.T, db *sql.DB, sessionID, ownerID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		sessionID, ownerID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func seedPersonaSession(t *testing.T, db *sql.DB, sessionID, ownerID, agentID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, agent_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		sessionID, ownerID, agentID, `{}`, "2026-09-12T12:00:00Z", "2026-09-12T12:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func testMemoryDocument(id, session, kind string) *store.MemoryDocument {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	return &store.MemoryDocument{
		ID: id, OwnerID: "owner-1", SessionID: session, Kind: kind,
		FromSequence: 1, ToSequence: 1, Content: "the nightly backup finished",
		TrustLabel: "owner_input", CreatedAt: now,
	}
}

func testProfileFact(id string) *profile.Fact {
	now := time.Now().UTC().Truncate(time.Second)
	return &profile.Fact{
		ID: id, OwnerID: "owner-1", Category: "language", Key: "backend",
		Value: "Go", ValueDigest: "digest-go", Status: "candidate", Origin: "derived",
		Confidence: 0.5, ConfidencePolicy: "conf-v1", CreatedAt: now, UpdatedAt: now,
	}
}

func seedErasureFixture(t *testing.T, db *sql.DB, root string) {
	t.Helper()
	ctx := t.Context()
	seedPersonaSession(t, db, "lobby/discord:owner/dm:601", "discord:601", "lobby")
	seedPersonaSession(t, db, "lobby/discord:owner/channel:333/principal:discord:601", "discord:601", "lobby")
	seedPersonaSession(t, db, "lobby/discord:owner/thread:7001", "discord:602", "lobby")
	seedPersonaSession(t, db, "main/discord:owner/dm:777", "local-owner", "main")
	memories := store.NewMemoryStore(db)
	for id, doc := range map[string]struct {
		owner, session string
		sequence       int64
	}{
		"mem-601-dm":     {"discord:601", "lobby/discord:owner/dm:601", 1},
		"mem-601-thread": {"discord:601", "lobby/discord:owner/thread:7001", 1},
		"mem-602-thread": {"discord:602", "lobby/discord:owner/thread:7001", 2},
		"mem-owner":      {"local-owner", "main/discord:owner/dm:777", 1},
	} {
		document := testMemoryDocument(id, doc.session, store.MemoryKindEventText)
		document.OwnerID = doc.owner
		document.FromSequence, document.ToSequence = doc.sequence, doc.sequence
		if _, err := memories.UpsertDocument(ctx, document); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", id, err)
		}
	}
	artifacts := storage.NewArtifactStore(db, root, storage.DefaultArtifactQuotaBytes)
	if _, err := artifacts.Put(ctx, strings.NewReader("parcel photo"), &storage.ArtifactMetadata{ID: "art-601", SessionID: "lobby/discord:owner/dm:601", Filename: "parcel.png", MediaType: "image/png"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := artifacts.Put(ctx, strings.NewReader("owner photo"), &storage.ArtifactMetadata{ID: "art-owner", SessionID: "main/discord:owner/dm:777", Filename: "owner.png", MediaType: "image/png"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	now := "2026-11-02T09:00:00Z"
	for _, stmt := range []string{
		`INSERT INTO principal_grant (principal_id, account, class, granted_at, granted_by) VALUES ('discord:601', 'owner', 'trusted', '` + now + `', 'local-owner')`,
		`INSERT INTO standing_grant (id, principal_id, agent_id, tool_name, granted_at, expires_at, granted_by) VALUES ('sg-1', 'discord:601', 'lobby', 'read_file', '` + now + `', '2026-11-09T09:00:00Z', 'local-owner')`,
		`INSERT INTO pairing_request (code_sha256, principal_id, account, created_at, expires_at) VALUES ('c1', 'discord:601', 'owner', '` + now + `', '2026-11-02T10:00:00Z')`,
		`INSERT INTO usage_reservation (id, invocation_id, attempt, model_definition_id, window_day, window_month, reserved_cost_micros, state, expires_at, created_at, updated_at, principal_id, principal_class)
			VALUES ('r-1', 'inv-1', 0, 'primary', '2026-11-02', '2026-11', 10, 'active', '` + now + `', '` + now + `', '` + now + `', 'discord:601', 'public')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func seedEmailEffect(t *testing.T, db *sql.DB, id, session, provider, operation, request string) {
	t.Helper()
	const at = "2026-11-06T09:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_intent (id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation, classification, state, request_digest, request_json, prepared_at, updated_at)
		VALUES (?, ?, 'turn-1', ?, ?, ?, ?, 'effectful', 'succeeded', 'digest', ?, ?, ?)`, id, session, id, "key-"+id, provider, operation, request, at, at); err != nil {
		t.Fatalf("seed effect %s: %v", id, err)
	}
}

type durableRunFixture struct {
	key, handler, payload, state string
	actions                      int
}

func seedDurableRun(t *testing.T, db *sql.DB, run durableRunFixture) {
	t.Helper()
	const at = "2026-11-12T09:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		run.key, run.handler, []byte(run.payload), run.state, at, at); err != nil {
		t.Fatalf("seed run %s: %v", run.key, err)
	}
	for i := range run.actions {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, created_at) VALUES (?, ?, 'result', ?, ?)`,
			run.key, fmt.Sprintf("step-%d", i), []byte(`{"text":"model reply"}`), at); err != nil {
			t.Fatalf("seed action of %s: %v", run.key, err)
		}
	}
}

type turnEventFixture struct {
	session, turn, kind string
}

func seedTurnEvent(t *testing.T, db *sql.DB, event turnEventFixture) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runtime_event (id, session_id, sequence, turn_id, invocation_id, author, kind, schema_version, payload_json, created_at)
		VALUES (?1 || '-' || ?3, ?2, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM runtime_event WHERE session_id = ?2), ?1, '', 'discord:601', ?3, 1, '{}', '2026-11-12T09:00:00Z')`,
		event.turn, event.session, event.kind); err != nil {
		t.Fatalf("seed %s of %s: %v", event.kind, event.turn, err)
	}
}

type seededRun struct {
	id, definition, principal, status, durableKey, parent, originSession string
	steps                                                                []string
}

func seedWorkflowDefinition(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_definition (id, version, goal, source, spec_json, spec_sha256, created_at)
		VALUES (?, 1, 'goal', 'composed', '{}', 'digest', ?)`, id, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed definition %s: %v", id, err)
	}
}

func seedWorkflowRun(t *testing.T, db *sql.DB, run *seededRun) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_run (id, definition_id, definition_version, durable_key, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id, origin_session_id, reply_account, reply_channel_id, parent_run_id)
		VALUES (?1, ?2, 1, NULLIF(?3, ''), 'goal', '{"objective":"private"}', ?4, ?5, ?5, ?6, 'trusted', 'main', NULLIF(?7, ''), 'acct', 'chan', NULLIF(?8, ''))`,
		run.id, run.definition, run.durableKey, run.status, erasureWorkflowStamp, run.principal, run.originSession, run.parent); err != nil {
		t.Fatalf("seed run %s: %v", run.id, err)
	}
	for _, step := range run.steps {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_step_run (run_id, step_id, status, attempt, updated_at) VALUES (?, ?, 'succeeded', 1, ?)`,
			run.id, step, erasureWorkflowStamp); err != nil {
			t.Fatalf("seed step %s/%s: %v", run.id, step, err)
		}
	}
}

func seedProposalApproval(t *testing.T, db *sql.DB, proposal, session, definition string) {
	t.Helper()
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_proposal (id, session_id, agent_id, principal_id, spec_json, spec_sha256, status, created_at)
		VALUES (?, ?, 'main', 'local-owner', '{}', 'digest', 'approved', ?)`, proposal, session, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed proposal %s: %v", proposal, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_approval (definition_id, definition_version, spec_sha256, kind, agent_id, approver_id, proposal_id, approved_at)
		VALUES (?, 1, 'digest', 'proposal', 'main', 'local-owner', ?, ?)`, definition, proposal, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed proposal approval %s: %v", proposal, err)
	}
}

func seedRunEvidence(t *testing.T, db *sql.DB, run *seededRun) {
	t.Helper()
	seedDurableRun(t, db, durableRunFixture{key: run.durableKey, handler: "workflow", payload: `{}`, state: "succeeded", actions: 1})
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_signal (run_key, name, seq, payload, created_at) VALUES (?, 'approval', 0, ?, ?)`,
		run.durableKey, []byte(`{}`), erasureWorkflowStamp); err != nil {
		t.Fatalf("seed signal: %v", err)
	}
}

func seedLegacyStepSession(t *testing.T, db *sql.DB, runID, stepID string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(runID + "/" + stepID))
	id := "wf-" + hex.EncodeToString(sum[:])[:24]
	if _, err := db.ExecContext(t.Context(), `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?, 'workflow', '{}', ?, ?)`,
		id, erasureWorkflowStamp, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	seedTurnEvent(t, db, turnEventFixture{session: id, turn: "turn-" + id, kind: "turn.accepted"})
	seedEmailEffect(t, db, "effect-"+id, id, "builtin-tools", "write_file", `{"arguments":{}}`)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_approval (id, intent_id, action, request_digest, owner_id, reason, token_hash, issued_at, expires_at)
		VALUES (?, ?, 'retry', 'digest', 'local-owner', 'r', ?, ?, ?)`, "approval-"+id, "effect-"+id, "token-"+id, erasureWorkflowStamp, erasureWorkflowStamp); err != nil {
		t.Fatalf("seed effect approval: %v", err)
	}
	return id
}

func workflowDataRows(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows := map[string]int{}
	for _, table := range []string{"session", "runtime_event", "effect_intent", "workflow_run", "workflow_step_run", "workflow_proposal", "workflow_approval", "workflow_definition", "durable_run", "durable_action", "durable_signal"} {
		rows[table] = countRows(t, db, `SELECT COUNT(*) FROM `+table)
	}
	return rows
}

func requireStoreErasureCode(t *testing.T, err error, want principal.ErrorCode) {
	t.Helper()
	if code, ok := fault.CodeOf[principal.ErrorCode](err); !ok || code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}
