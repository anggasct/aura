package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type UpgradeResult struct {
	Applied int
	Latest  int
	Backup  string
	Report  RestoreReport
}

func UpgradePath(ctx context.Context, dbPath string, opts sqlite.Options, migrations []sqlite.Migration, backupRoot, artifactRoot string, now time.Time) (result UpgradeResult, retErr error) {
	if err := sqlite.CheckCompatibility(ctx, dbPath); err != nil {
		return UpgradeResult{}, err
	}
	db, err := sqlite.Open(ctx, dbPath, opts)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	return UpgradeWithBackup(ctx, db, migrations, backupRoot, artifactRoot, now)
}

func UpgradeWithBackup(ctx context.Context, db *sql.DB, migrations []sqlite.Migration, backupRoot, artifactRoot string, now time.Time) (UpgradeResult, error) {
	applied, latest, err := sqlite.SchemaVersions(ctx, db, migrations)
	if err != nil {
		return UpgradeResult{}, err
	}
	result := UpgradeResult{Applied: applied, Latest: latest}
	if applied > latest {
		return result, fault.New(sqlite.ErrorCodeMigrationSequenceInvalid, fmt.Sprintf("database schema version %d is newer than this binary supports (%d)", applied, latest))
	}
	if applied == latest {
		return result, nil
	}
	result.Backup = fmt.Sprintf("pre-upgrade-v%d-v%d-%s", applied, latest, now.UTC().Format("20060102T150405Z"))
	dest := filepath.Join(backupRoot, result.Backup)
	if _, err := Backup(ctx, db, dest); err != nil {
		return result, err
	}
	report, err := VerifyRestore(ctx, dest, artifactRoot)
	if err != nil {
		return result, err
	}
	result.Report = report
	if len(report.ChecksumMismatches) > 0 || len(report.MissingBlobFiles) > 0 {
		return result, Errorf(ErrorCodeBackupInvalid, "pre-upgrade backup %s failed verification: %d checksum mismatches, %d missing blobs", result.Backup, len(report.ChecksumMismatches), len(report.MissingBlobFiles))
	}
	if err := sqlite.Migrate(ctx, db, migrations); err != nil {
		return result, err
	}
	return result, nil
}
