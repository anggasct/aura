package history

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

func newMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, Migrations()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestMigrateAppliesFoundationalSchema(t *testing.T) {
	db := newMigratedDB(t)

	for _, table := range []string{"schema_migration", "session", "runtime_event", "blob", "artifact_ref", "ingress_dedupe"} {
		var name string
		err := db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)

	if err := sqlite.Migrate(ctx, db, Migrations()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigrateChecksumMismatch(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)

	if _, err := db.ExecContext(ctx, `UPDATE schema_migration SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}

	err := sqlite.Migrate(ctx, db, Migrations())
	if err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
	code, ok := fault.CodeOf[sqlite.ErrorCode](err)
	if !ok || code != sqlite.ErrorCodeMigrationChecksumMismatch {
		t.Fatalf("CodeOf(err) = %v, %v; want %s", code, ok, sqlite.ErrorCodeMigrationChecksumMismatch)
	}
}

func TestMigrateRejectsVersionAboveBinary(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)
	migrations := Migrations()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migration (version, checksum, applied_at) VALUES (?, ?, ?)`,
		migrations[len(migrations)-1].Version+1, "future", "2026-08-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}

	err := sqlite.Migrate(ctx, db, migrations)
	if !fault.HasCode(err, sqlite.ErrorCodeMigrationSequenceInvalid) {
		t.Fatalf("Migrate = %v, want %s", err, sqlite.ErrorCodeMigrationSequenceInvalid)
	}
}
