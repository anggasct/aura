package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{version: 1, sql: foundationalSchemaSQL},
	{version: 2, sql: usageLedgerSchemaSQL},
	{version: 3, sql: effectIntentSchemaSQL},
	{version: 4, sql: effectApprovalSchemaSQL},
	{version: 5, sql: workflowSchemaSQL},
	{version: 6, sql: modelCircuitCheckpointSchemaSQL},
	{version: 7, sql: webhookExecutionSchemaSQL},
	{version: 8, sql: workflowCorrelationSchemaSQL},
	{version: 9, sql: channelResumeSchemaSQL},
	{version: 10, sql: broadcastItemSchemaSQL},
	{version: 11, sql: broadcastDestinationSchemaSQL},
	{version: 12, sql: scheduleSchemaSQL},
	{version: 13, sql: memoryDocumentSchemaSQL},
	{version: 14, sql: skillPackageSchemaSQL},
	{version: 15, sql: profileSchemaSQL},
	{version: 16, sql: childRunSchemaSQL},
	{version: 17, sql: childRunDepthRemovalSQL},
	{version: 18, sql: childRunResultSchemaSQL},
	{version: 19, sql: childRunTypedResultSchemaSQL},
}

const bootstrapSchemaMigrationTableSQL = `
CREATE TABLE IF NOT EXISTS schema_migration (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);`

const foundationalSchemaSQL = `
CREATE TABLE session (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE runtime_event (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    turn_id TEXT NOT NULL,
    invocation_id TEXT NOT NULL,
    branch TEXT NOT NULL DEFAULT '',
    author TEXT NOT NULL,
    kind TEXT NOT NULL,
    schema_version INTEGER NOT NULL CHECK (schema_version > 0),
    payload_json TEXT NOT NULL,
    provider_usage_json TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (session_id, sequence)
);

CREATE INDEX runtime_event_turn_idx
    ON runtime_event(session_id, turn_id, sequence);

CREATE TABLE blob (
    digest TEXT PRIMARY KEY,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    media_type TEXT NOT NULL,
    relative_path TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE artifact_ref (
    id TEXT PRIMARY KEY,
    blob_digest TEXT NOT NULL REFERENCES blob(digest) ON DELETE RESTRICT,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    event_id TEXT REFERENCES runtime_event(id) ON DELETE SET NULL,
    filename TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX artifact_ref_session_idx
    ON artifact_ref(session_id, created_at, id);

CREATE TABLE ingress_dedupe (
    source TEXT NOT NULL,
    external_id TEXT NOT NULL,
    accepted_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    PRIMARY KEY (source, external_id)
);
`

const usageLedgerSchemaSQL = `
CREATE TABLE usage_reservation (
    id TEXT PRIMARY KEY,
    invocation_id TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    model_definition_id TEXT NOT NULL,
    window_day TEXT NOT NULL,
    window_month TEXT NOT NULL,
    reserved_cost_micros INTEGER NOT NULL CHECK (reserved_cost_micros >= 0),
    state TEXT NOT NULL CHECK (state IN ('active','settled','expired','reconciled')),
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(invocation_id, attempt)
);

CREATE TABLE usage_entry (
    id TEXT PRIMARY KEY,
    reservation_id TEXT NOT NULL UNIQUE REFERENCES usage_reservation(id) ON DELETE RESTRICT,
    provider_usage_id TEXT,
    input_tokens INTEGER NOT NULL CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK (output_tokens >= 0),
    usage_json TEXT NOT NULL,
    cost_micros INTEGER NOT NULL CHECK (cost_micros >= 0),
    accounting TEXT NOT NULL CHECK (accounting IN ('reported','estimated','reconciled')),
    price_version TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

CREATE INDEX usage_window_idx ON usage_reservation(window_day, window_month, state);
`

const effectIntentSchemaSQL = `
CREATE TABLE effect_intent (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    turn_id TEXT NOT NULL,
    tool_call_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    provider TEXT NOT NULL,
    operation TEXT NOT NULL,
    classification TEXT NOT NULL
        CHECK (classification IN ('read_only','idempotent','effectful','irreversible')),
    state TEXT NOT NULL
        CHECK (state IN ('prepared','started','succeeded','unknown','failed')),
    request_digest TEXT NOT NULL,
    request_json TEXT NOT NULL,
    provider_receipt_json TEXT,
    safe_error_code TEXT,
    retry_of TEXT REFERENCES effect_intent(id) ON DELETE SET NULL,
    prepared_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    reconciled_at TEXT,
    updated_at TEXT NOT NULL,
    UNIQUE (provider, operation, idempotency_key)
);

CREATE INDEX effect_intent_recovery_idx
    ON effect_intent(state, updated_at);

CREATE INDEX effect_intent_turn_idx
    ON effect_intent(session_id, turn_id, prepared_at);
`

const effectApprovalSchemaSQL = `
CREATE TABLE effect_approval (
    id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL REFERENCES effect_intent(id) ON DELETE RESTRICT,
    action TEXT NOT NULL
        CHECK (action IN ('mark_succeeded','mark_failed','retry')),
    request_digest TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    issued_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);

CREATE INDEX effect_approval_intent_idx
    ON effect_approval(intent_id, issued_at);
`

const workflowSchemaSQL = `
CREATE TABLE workflow_definition (
    id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    goal TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('defined','composed','generated')),
    spec_json TEXT NOT NULL CHECK (json_valid(spec_json)),
    spec_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (id, version)
);

CREATE TABLE workflow_run (
    id TEXT PRIMARY KEY,
    definition_id TEXT NOT NULL,
    definition_version INTEGER NOT NULL,
    durable_key TEXT UNIQUE,
    goal TEXT NOT NULL,
    input_json TEXT NOT NULL CHECK (json_valid(input_json)),
    status TEXT NOT NULL CHECK (
        status IN ('queued','running','suspended','succeeded','failed','cancelled')
    ),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (definition_id, definition_version)
        REFERENCES workflow_definition(id, version)
        ON DELETE RESTRICT
);

CREATE INDEX workflow_run_status_idx ON workflow_run(status, created_at);

CREATE TABLE workflow_step_run (
    run_id TEXT NOT NULL REFERENCES workflow_run(id) ON DELETE CASCADE,
    step_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('pending','ready','running','succeeded','failed','skipped')
    ),
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    started_at TEXT,
    ended_at TEXT,
    output_json TEXT CHECK (
        output_json IS NULL
        OR (json_valid(output_json) AND length(output_json) <= 65536)
    ),
    output_artifact_digest TEXT,
    error_code TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, step_id)
);
`

const webhookExecutionSchemaSQL = `
CREATE TABLE webhook_execution (
    id TEXT PRIMARY KEY,
    key_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    nonce TEXT NOT NULL,
    body_digest TEXT NOT NULL,
    turn_id TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('accepted','running','completed','failed','cancelled')),
    result_event_id TEXT REFERENCES runtime_event(id) ON DELETE SET NULL,
    error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    UNIQUE (key_id, nonce),
    UNIQUE (key_id, event_id)
);

CREATE INDEX webhook_execution_expiry_idx
    ON webhook_execution(expires_at);
`

const workflowCorrelationSchemaSQL = `
CREATE TABLE workflow_correlation (
    source TEXT NOT NULL,
    event_type TEXT NOT NULL,
    external_id TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES workflow_run(id) ON DELETE CASCADE,
    signal_name TEXT NOT NULL,
    dedupe_key TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (source, event_type, external_id, dedupe_key)
);

CREATE INDEX workflow_correlation_run_idx ON workflow_correlation(run_id);
`

const channelResumeSchemaSQL = `
CREATE TABLE channel_resume (
    source TEXT NOT NULL,
    instance TEXT NOT NULL,
    gateway_session_id TEXT NOT NULL,
    last_sequence INTEGER NOT NULL CHECK (last_sequence >= 0),
    config_digest TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (source, instance)
);
`

const broadcastItemSchemaSQL = `
CREATE TABLE broadcast_item (
    id TEXT PRIMARY KEY,
    producer TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    priority TEXT NOT NULL CHECK (priority IN ('urgent','warning','info')),
    destination_alias TEXT NOT NULL,
    content_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('held','scheduled','started','succeeded','failed','unknown','cancelled')),
    not_before TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    effect_id TEXT REFERENCES effect_intent(id) ON DELETE RESTRICT,
    digest_parent_id TEXT REFERENCES broadcast_item(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(producer, idempotency_key)
);
CREATE INDEX broadcast_due_idx ON broadcast_item(state, not_before, priority, created_at, id);
`

const broadcastDestinationSchemaSQL = `
CREATE TABLE broadcast_destination (
    alias TEXT PRIMARY KEY,
    next_eligible_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`

const scheduleSchemaSQL = `
CREATE TABLE scheduled_job (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    cron_expression TEXT NOT NULL,
    timezone TEXT NOT NULL,
    prompt TEXT NOT NULL,
    origin_channel TEXT NOT NULL,
    origin_destination TEXT NOT NULL,
    overlap_policy TEXT NOT NULL
        CHECK (overlap_policy IN ('skip','queue_one')),
    catch_up_grace_seconds INTEGER NOT NULL CHECK (catch_up_grace_seconds >= 0),
    state TEXT NOT NULL CHECK (state IN ('active','paused','deleted')),
    version INTEGER NOT NULL CHECK (version > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE scheduled_occurrence (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES scheduled_job(id) ON DELETE CASCADE,
    job_version INTEGER NOT NULL,
    scheduled_for_utc TEXT NOT NULL,
    state TEXT NOT NULL CHECK (
        state IN ('fired','completed','failed',
                  'expired','skipped_overlap','cancelled')
    ),
    turn_id TEXT UNIQUE,
    result_event_id TEXT REFERENCES runtime_event(id) ON DELETE SET NULL,
    safe_error_code TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (job_id, scheduled_for_utc)
);
CREATE INDEX scheduled_occurrence_job_idx
    ON scheduled_occurrence(job_id, scheduled_for_utc);
`

const modelCircuitCheckpointSchemaSQL = `
CREATE TABLE model_circuit_checkpoint (
    circuit_key TEXT PRIMARY KEY,
    config_digest TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('closed','open','half_open')),
    consecutive_failures INTEGER NOT NULL CHECK (consecutive_failures >= 0),
    open_until TEXT,
    updated_at TEXT NOT NULL
);
`

const memoryDocumentSchemaSQL = `
CREATE TABLE memory_document (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('event_text','summary')),
    source_from_sequence INTEGER NOT NULL CHECK (source_from_sequence > 0),
    source_to_sequence INTEGER NOT NULL
        CHECK (source_to_sequence >= source_from_sequence),
    content TEXT NOT NULL,
    trust_label TEXT NOT NULL
        CHECK (trust_label IN ('owner_input','untrusted_external','derived_untrusted')),
    prompt_version TEXT,
    model_protocol TEXT,
    model_name TEXT,
    created_at TEXT NOT NULL,
    expires_at TEXT,
    UNIQUE (session_id, kind, source_from_sequence, source_to_sequence, prompt_version)
);
CREATE INDEX memory_document_source_idx
    ON memory_document(session_id, source_from_sequence, source_to_sequence);
CREATE VIRTUAL TABLE memory_document_fts USING fts5(
    content,
    content='memory_document',
    content_rowid='rowid',
    tokenize='unicode61'
);
CREATE TRIGGER memory_document_fts_insert AFTER INSERT ON memory_document BEGIN
    INSERT INTO memory_document_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER memory_document_fts_delete AFTER DELETE ON memory_document BEGIN
    INSERT INTO memory_document_fts(memory_document_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER memory_document_fts_update AFTER UPDATE OF content ON memory_document BEGIN
    INSERT INTO memory_document_fts(memory_document_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
    INSERT INTO memory_document_fts(rowid, content) VALUES (new.rowid, new.content);
END;
`

const skillPackageSchemaSQL = `
CREATE TABLE skill_package (
    id TEXT PRIMARY KEY,
    canonical_name TEXT NOT NULL,
    origin_json TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('quarantined','active','disabled','rejected','conflict')),
    validation_json TEXT NOT NULL,
    requested_capabilities_json TEXT NOT NULL DEFAULT '[]',
    granted_capabilities_json TEXT NOT NULL DEFAULT '[]',
    reviewed_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (canonical_name, origin_json, content_digest)
);
CREATE INDEX skill_state_name_idx
    ON skill_package(state, canonical_name, id);
`

const profileSchemaSQL = `
CREATE TABLE profile_fact (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    category TEXT NOT NULL CHECK (category IN ('preference','tool','language','timezone','project','habit')),
    fact_key TEXT NOT NULL,
    fact_value TEXT NOT NULL,
    value_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('candidate','active','rejected','expired','deleted')),
    origin TEXT NOT NULL CHECK (origin IN ('derived','owner')),
    owner_verified INTEGER NOT NULL DEFAULT 0 CHECK (owner_verified IN (0,1)),
    confidence REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    confidence_policy_version TEXT NOT NULL,
    conflicts_with_id TEXT REFERENCES profile_fact(id) ON DELETE SET NULL,
    expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(owner_id, category, fact_key, value_digest)
);
CREATE INDEX profile_fact_lookup_idx
    ON profile_fact(owner_id, status, category, fact_key, expires_at, confidence DESC, id);
CREATE TABLE profile_evidence (
    fact_id TEXT NOT NULL REFERENCES profile_fact(id) ON DELETE CASCADE,
    source_event_id TEXT NOT NULL REFERENCES runtime_event(id) ON DELETE RESTRICT,
    source_digest TEXT NOT NULL,
    extractor_provider TEXT,
    extractor_model TEXT,
    extractor_model_version TEXT,
    prompt_version TEXT,
    prompt_digest TEXT,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (fact_id, source_event_id, source_digest)
);
CREATE VIRTUAL TABLE profile_fact_fts USING fts5(
    fact_key,
    fact_value,
    content='profile_fact',
    content_rowid='rowid',
    tokenize='unicode61'
);
CREATE TRIGGER profile_fact_fts_insert AFTER INSERT ON profile_fact BEGIN
    INSERT INTO profile_fact_fts(rowid, fact_key, fact_value) VALUES (new.rowid, new.fact_key, new.fact_value);
END;
CREATE TRIGGER profile_fact_fts_delete AFTER DELETE ON profile_fact BEGIN
    INSERT INTO profile_fact_fts(profile_fact_fts, rowid, fact_key, fact_value) VALUES ('delete', old.rowid, old.fact_key, old.fact_value);
END;
CREATE TRIGGER profile_fact_fts_update AFTER UPDATE OF fact_key, fact_value ON profile_fact BEGIN
    INSERT INTO profile_fact_fts(profile_fact_fts, rowid, fact_key, fact_value) VALUES ('delete', old.rowid, old.fact_key, old.fact_value);
    INSERT INTO profile_fact_fts(rowid, fact_key, fact_value) VALUES (new.rowid, new.fact_key, new.fact_value);
END;
`

const childRunSchemaSQL = `
CREATE TABLE child_run (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    parent_session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
    parent_turn_id TEXT NOT NULL,
    parent_invocation_id TEXT NOT NULL,
    child_session_id TEXT NOT NULL UNIQUE REFERENCES session(id) ON DELETE CASCADE,
    depth INTEGER NOT NULL,
    durable_key TEXT NOT NULL,
    context_digest TEXT NOT NULL,
    grants_json TEXT NOT NULL,
    budget_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed','cancelled','deadline_exceeded','interrupted')),
    deadline TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(parent_invocation_id, idempotency_key)
);
CREATE INDEX child_parent_state_idx ON child_run(parent_session_id, state, created_at, id);
`

const childRunDepthRemovalSQL = `
ALTER TABLE child_run DROP COLUMN depth;
`

const childRunResultSchemaSQL = `
ALTER TABLE child_run ADD COLUMN result_status TEXT;
ALTER TABLE child_run ADD COLUMN result_output TEXT;
ALTER TABLE child_run ADD COLUMN result_artifacts_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE child_run ADD COLUMN tokens_used INTEGER NOT NULL DEFAULT 0;
ALTER TABLE child_run ADD COLUMN cost_micros INTEGER NOT NULL DEFAULT 0;
ALTER TABLE child_run ADD COLUMN completed_at TEXT;
ALTER TABLE child_run ADD COLUMN result_provenance TEXT;
`

const childRunTypedResultSchemaSQL = `
ALTER TABLE child_run ADD COLUMN result_child_id TEXT;
ALTER TABLE child_run ADD COLUMN result_session_id TEXT;
ALTER TABLE child_run ADD COLUMN result_context_digest TEXT;
ALTER TABLE child_run ADD COLUMN result_durable_key TEXT;
ALTER TABLE child_run ADD COLUMN result_source_range TEXT;
ALTER TABLE child_run ADD COLUMN result_model TEXT;
ALTER TABLE child_run ADD COLUMN result_prompt_version TEXT;
ALTER TABLE child_run ADD COLUMN result_trust TEXT;
`

func Migrate(ctx context.Context, db *sql.DB) error {
	if err := validateMigrationOrder(migrations); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		return fmt.Errorf("bootstrap schema_migration table: %w", err)
	}
	var appliedMax sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migration`).Scan(&appliedMax); err != nil {
		return fmt.Errorf("read applied migration state: %w", err)
	}
	maxVersion := migrations[len(migrations)-1].version
	if appliedMax.Valid && int(appliedMax.Int64) > maxVersion {
		return &Error{
			Code:   ErrorCodeMigrationSequenceInvalid,
			Detail: fmt.Sprintf("database schema version %d is newer than this binary supports (%d)", appliedMax.Int64, maxVersion),
		}
	}
	for _, m := range migrations {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func SchemaVersions(ctx context.Context, db *sql.DB) (applied, latest int, err error) {
	latest = migrations[len(migrations)-1].version
	var appliedMax sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migration`).Scan(&appliedMax); err != nil {
		return 0, 0, fmt.Errorf("read applied migration state: %w", err)
	}
	if appliedMax.Valid {
		applied = int(appliedMax.Int64)
	}
	return applied, latest, nil
}

func validateMigrationOrder(ms []migration) error {
	for i := 1; i < len(ms); i++ {
		if ms[i].version <= ms[i-1].version {
			return &Error{
				Code:   ErrorCodeMigrationSequenceInvalid,
				Detail: fmt.Sprintf("migration %d follows %d, versions must be strictly increasing", ms[i].version, ms[i-1].version),
			}
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	checksum := checksumOf(m.sql)

	var stored string
	err := db.QueryRowContext(ctx, `SELECT checksum FROM schema_migration WHERE version = ?`, m.version).Scan(&stored)
	switch {
	case err == nil:
		if stored != checksum {
			return &Error{
				Code:   ErrorCodeMigrationChecksumMismatch,
				Detail: fmt.Sprintf("migration %d: stored checksum %s does not match %s", m.version, stored, checksum),
			}
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("read migration %d state: %w", m.version, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %d: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migration (version, checksum, applied_at) VALUES (?, ?, ?)`,
		m.version, checksum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	return tx.Commit()
}

func checksumOf(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:])
}
