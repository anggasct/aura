package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

const bootstrapSchemaMigrationTableSQL = `
CREATE TABLE IF NOT EXISTS schema_migration (
    version INTEGER PRIMARY KEY,
    owner TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);`

type Migration struct {
	Version int
	Owner   string
	SQL     string
}

func Migrate(ctx context.Context, db *sql.DB, history []Migration) error {
	if db == nil {
		return fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if len(history) == 0 {
		return fault.New(ErrorCodeInvalidArgument, "migration history must not be empty")
	}
	if err := validateMigrationOrder(history); err != nil {
		return err
	}
	for _, m := range history {
		if strings.TrimSpace(m.Owner) == "" {
			return fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("migration %d owner must not be empty", m.Version))
		}
	}
	if err := checkDBCompatibility(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, bootstrapSchemaMigrationTableSQL); err != nil {
		return Classify(fmt.Errorf("bootstrap schema_migration table: %w", err))
	}
	var appliedMax sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migration`).Scan(&appliedMax); err != nil {
		return Classify(fmt.Errorf("read applied migration state: %w", err))
	}
	maxVersion := history[len(history)-1].Version
	if appliedMax.Valid && int(appliedMax.Int64) > maxVersion {
		return fault.New(ErrorCodeMigrationSequenceInvalid,
			fmt.Sprintf("database schema version %d is newer than this binary supports (%d)", appliedMax.Int64, maxVersion))
	}
	for _, m := range history {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func SchemaVersions(ctx context.Context, db *sql.DB, history []Migration) (applied, latest int, err error) {
	if db == nil {
		return 0, 0, fault.New(ErrorCodeInvalidArgument, "database must not be nil")
	}
	if len(history) == 0 {
		return 0, 0, fault.New(ErrorCodeInvalidArgument, "migration history must not be empty")
	}
	if err := checkDBCompatibility(ctx, db); err != nil {
		return 0, 0, err
	}
	latest = history[len(history)-1].Version
	var appliedMax sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migration`).Scan(&appliedMax); err != nil {
		return 0, 0, Classify(fmt.Errorf("read applied migration state: %w", err))
	}
	if appliedMax.Valid {
		applied = int(appliedMax.Int64)
	}
	return applied, latest, nil
}

func Checksum(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:])
}

func validateMigrationOrder(history []Migration) error {
	if len(history) == 0 {
		return nil
	}
	if history[0].Version <= 0 {
		return fault.New(ErrorCodeMigrationSequenceInvalid,
			fmt.Sprintf("migration %d must be positive", history[0].Version))
	}
	for i := 1; i < len(history); i++ {
		if history[i].Version <= history[i-1].Version {
			return fault.New(ErrorCodeMigrationSequenceInvalid,
				fmt.Sprintf("migration %d follows %d, versions must be strictly increasing", history[i].Version, history[i-1].Version))
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	if strings.TrimSpace(m.Owner) == "" {
		return fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("migration %d owner must not be empty", m.Version))
	}
	checksum := Checksum(m.SQL)

	var storedOwner, storedChecksum string
	err := db.QueryRowContext(ctx, `SELECT owner, checksum FROM schema_migration WHERE version = ?`, m.Version).Scan(&storedOwner, &storedChecksum)
	switch {
	case err == nil:
		if storedOwner != m.Owner {
			return fault.New(ErrorCodeMigrationChecksumMismatch,
				fmt.Sprintf("migration %d: stored owner %q does not match %q", m.Version, storedOwner, m.Owner))
		}
		if storedChecksum != checksum {
			return fault.New(ErrorCodeMigrationChecksumMismatch,
				fmt.Sprintf("migration %d: stored checksum %s does not match %s", m.Version, storedChecksum, checksum))
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return Classify(fmt.Errorf("read migration %d state: %w", m.Version, err))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Classify(fmt.Errorf("begin migration %d: %w", m.Version, err))
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return Classify(fmt.Errorf("apply migration %d: %w", m.Version, err))
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migration (version, owner, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.Version, m.Owner, checksum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return Classify(fmt.Errorf("record migration %d: %w", m.Version, err))
	}
	return Classify(tx.Commit())
}
