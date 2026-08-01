package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	backupDatabaseFilename = "aura.db"
	backupManifestFilename = "manifest.json"
)

type BackupBlobEntry struct {
	Digest       string
	SizeBytes    int64
	RelativePath string
}

type BackupManifest struct {
	CreatedAt time.Time
	Blobs     []BackupBlobEntry
}

// Backup writes a consistent point-in-time SQLite snapshot plus a manifest
// of every blob digest, size, and path into destDir, so Restore can later
// verify recovered sessions, events, dedupe keys, and artifact links against
// checksummed content.
func Backup(ctx context.Context, db *sql.DB, destDir string) (BackupManifest, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup dir %s: %w", destDir, err)
	}

	dbDest := filepath.Join(destDir, backupDatabaseFilename)
	if _, err := os.Stat(dbDest); err == nil {
		return BackupManifest{}, fmt.Errorf("backup database already exists at %s", dbDest)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dbDest); err != nil {
		return BackupManifest{}, fmt.Errorf("online backup to %s: %w", dbDest, err)
	}

	rows, err := db.QueryContext(ctx, `SELECT digest, size_bytes, relative_path FROM blob ORDER BY digest`)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("list blobs for manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	manifest := BackupManifest{CreatedAt: time.Now().UTC()}
	for rows.Next() {
		var e BackupBlobEntry
		if err := rows.Scan(&e.Digest, &e.SizeBytes, &e.RelativePath); err != nil {
			return BackupManifest{}, fmt.Errorf("scan blob for manifest: %w", err)
		}
		manifest.Blobs = append(manifest.Blobs, e)
	}
	if err := rows.Err(); err != nil {
		return BackupManifest{}, fmt.Errorf("list blobs for manifest: %w", err)
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupManifest{}, fmt.Errorf("marshal backup manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, backupManifestFilename), data, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("write backup manifest: %w", err)
	}
	return manifest, nil
}

type RestoreReport struct {
	Sessions           int
	Events             int
	DedupeKeys         int
	ArtifactRefs       int
	VerifiedBlobs      int
	ChecksumMismatches []string
	MissingBlobFiles   []string
}

// VerifyRestore opens the database backed up under backupDir, confirms
// sessions, events, dedupe keys, and artifact links are present, and
// recomputes each manifest blob's checksum against artifactRoot. It never
// deletes or modifies data; it only reports findings.
func VerifyRestore(ctx context.Context, backupDir, artifactRoot string) (RestoreReport, error) {
	manifestData, err := os.ReadFile(filepath.Join(backupDir, backupManifestFilename))
	if err != nil {
		return RestoreReport{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest BackupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return RestoreReport{}, fmt.Errorf("parse backup manifest: %w", err)
	}

	db, err := OpenDB(ctx, filepath.Join(backupDir, backupDatabaseFilename))
	if err != nil {
		return RestoreReport{}, fmt.Errorf("open restored database: %w", err)
	}
	defer func() { _ = db.Close() }()

	var report RestoreReport
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session`).Scan(&report.Sessions); err != nil {
		return RestoreReport{}, fmt.Errorf("count sessions: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_event`).Scan(&report.Events); err != nil {
		return RestoreReport{}, fmt.Errorf("count events: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingress_dedupe`).Scan(&report.DedupeKeys); err != nil {
		return RestoreReport{}, fmt.Errorf("count dedupe keys: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_ref`).Scan(&report.ArtifactRefs); err != nil {
		return RestoreReport{}, fmt.Errorf("count artifact refs: %w", err)
	}

	for _, blob := range manifest.Blobs {
		absPath, err := resolveRootedPath(artifactRoot, blob.RelativePath)
		if err != nil {
			return RestoreReport{}, err
		}
		digest, err := checksumFile(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				report.MissingBlobFiles = append(report.MissingBlobFiles, blob.Digest)
				continue
			}
			return RestoreReport{}, fmt.Errorf("checksum blob %s: %w", blob.Digest, err)
		}
		if digest != blob.Digest {
			report.ChecksumMismatches = append(report.ChecksumMismatches, blob.Digest)
			continue
		}
		report.VerifiedBlobs++
	}
	return report, nil
}

func checksumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
