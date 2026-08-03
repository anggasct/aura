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

// Migrate applies the foundational schema and any registered feature
// migrations, verifying the checksum of migrations already recorded as
// applied. A checksum mismatch or an applied version above the binary's
// maximum stops startup without touching the schema.
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

// SchemaVersions reports the highest applied migration version and the highest
// version this binary knows about. A database with no migrations applied
// reports applied 0. Health checks compare the two to detect a pending
// migration (applied < latest) or a downgrade (applied > latest).
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
		// Not yet applied.
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
