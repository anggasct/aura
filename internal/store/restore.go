package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RestoreOptions controls offline restore behavior. Force is required to
// replace an existing live database.
type RestoreOptions struct {
	Force bool
}

// RestoreBackup installs a verified backup snapshot as the live database at
// liveDBPath. The backup is verified first (integrity, manifest, and blob
// checksums); any verification failure aborts before the live database is
// touched. Without Force, an existing live database is refused with
// restore_locked. The backup files are never modified.
func Restore(ctx context.Context, backupDir, artifactRoot, liveDBPath string, opts RestoreOptions) (RestoreReport, error) {
	report, err := VerifyRestore(ctx, backupDir, artifactRoot)
	if err != nil {
		return RestoreReport{}, err
	}
	if len(report.ChecksumMismatches) > 0 || len(report.MissingBlobFiles) > 0 {
		return RestoreReport{}, errBackupInvalid(fmt.Sprintf("backup verification found %d checksum mismatches and %d missing blobs", len(report.ChecksumMismatches), len(report.MissingBlobFiles)))
	}
	if !opts.Force {
		if _, err := os.Stat(liveDBPath); err == nil {
			return RestoreReport{}, &Error{Code: ErrorCodeRestoreLocked, Detail: "live database exists; restore requires --force"}
		} else if !os.IsNotExist(err) {
			return RestoreReport{}, fmt.Errorf("inspect live database path: %w", err)
		}
	}
	src := filepath.Join(backupDir, backupDatabaseFilename)
	if err := copyFileSynced(ctx, src, liveDBPath); err != nil {
		return RestoreReport{}, fmt.Errorf("restore live database: %w", err)
	}
	return report, nil
}

func errBackupInvalid(detail string) error {
	return &Error{Code: ErrorCodeBackupInvalid, Detail: detail}
}

func copyFileSynced(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	removeTmp = false
	return syncPath(dir)
}

// BackupStore is the spec contract for backup/restore operations. The
// concrete implementation composes the store's free functions so feature
// packages can depend on the interface without importing the driver.
type BackupStore interface {
	CreateBackup(ctx context.Context, destDir string) (BackupManifest, error)
	VerifyBackup(ctx context.Context, backupDir string) (RestoreReport, error)
	RestoreBackup(ctx context.Context, backupDir string, opts RestoreOptions) error
}

type sqliteBackupStore struct {
	db           *sql.DB
	artifactRoot string
	liveDBPath   string
}

func NewBackupStore(db *sql.DB, artifactRoot, liveDBPath string) BackupStore {
	return &sqliteBackupStore{db: db, artifactRoot: artifactRoot, liveDBPath: liveDBPath}
}

func (s *sqliteBackupStore) CreateBackup(ctx context.Context, destDir string) (BackupManifest, error) {
	return Backup(ctx, s.db, destDir)
}

func (s *sqliteBackupStore) VerifyBackup(ctx context.Context, backupDir string) (RestoreReport, error) {
	return VerifyRestore(ctx, backupDir, s.artifactRoot)
}

func (s *sqliteBackupStore) RestoreBackup(ctx context.Context, backupDir string, opts RestoreOptions) error {
	_, err := Restore(ctx, backupDir, s.artifactRoot, s.liveDBPath, opts)
	return err
}
