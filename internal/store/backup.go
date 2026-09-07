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
	"strings"
	"time"
)

const (
	backupDatabaseFilename = "aura.db"
	backupManifestFilename = "manifest.json"
)

type BackupBlobEntry struct {
	Digest       string `json:"Digest"`
	SizeBytes    int64  `json:"SizeBytes"`
	RelativePath string `json:"RelativePath"`
}

type BackupManifest struct {
	CreatedAt time.Time         `json:"CreatedAt"`
	Blobs     []BackupBlobEntry `json:"Blobs"`
}

func Backup(ctx context.Context, db *sql.DB, destDir string) (BackupManifest, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup directory: %w", err)
	}

	dbDest := filepath.Join(destDir, backupDatabaseFilename)
	if _, err := os.Stat(dbDest); err == nil {
		return BackupManifest{}, errBackupDestinationConflict()
	}

	tmp, err := os.CreateTemp(destDir, backupDatabaseFilename+".tmp-*")
	if err != nil {
		return BackupManifest{}, backupFail("create backup temp file", err, destDir)
	}
	tmpDest := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpDest)
		return BackupManifest{}, backupFail("close backup temp file", err, tmpDest)
	}
	if err := os.Remove(tmpDest); err != nil {
		return BackupManifest{}, backupFail("prepare backup temp file", err, tmpDest)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmpDest); err != nil {
		_ = os.Remove(tmpDest)
		return BackupManifest{}, backupFail("create backup snapshot", err, tmpDest)
	}
	if err := syncPath(tmpDest); err != nil {
		_ = os.Remove(tmpDest)
		return BackupManifest{}, backupFail("fsync backup snapshot", err, tmpDest)
	}
	if _, err := os.Stat(dbDest); err == nil {
		_ = os.Remove(tmpDest)
		return BackupManifest{}, errBackupDestinationConflict()
	}
	if err := os.Rename(tmpDest, dbDest); err != nil {
		_ = os.Remove(tmpDest)
		return BackupManifest{}, backupFail("move backup snapshot into place", err, tmpDest, dbDest)
	}
	if err := os.Chmod(dbDest, 0o600); err != nil {
		return BackupManifest{}, backupFail("secure backup snapshot", err, dbDest)
	}
	if err := syncPath(destDir); err != nil {
		return BackupManifest{}, backupFail("fsync backup directory", err, destDir)
	}

	snap, err := openReadOnly(ctx, dbDest)
	if err != nil {
		return BackupManifest{}, backupFail("open backup snapshot", err, dbDest)
	}
	defer func() { _ = snap.Close() }()

	rows, err := snap.QueryContext(ctx, `SELECT digest, size_bytes, relative_path FROM blob ORDER BY digest`)
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
	if err := writeSynced(filepath.Join(destDir, backupManifestFilename+".tmp"), filepath.Join(destDir, backupManifestFilename), data); err != nil {
		return BackupManifest{}, backupFail("write backup manifest", err, filepath.Join(destDir, backupManifestFilename), filepath.Join(destDir, backupManifestFilename+".tmp"))
	}
	if err := syncPath(destDir); err != nil {
		return BackupManifest{}, backupFail("fsync backup directory", err, destDir)
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

func ReadBackupManifest(backupDir string) (BackupManifest, error) {
	path := filepath.Join(backupDir, backupManifestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return BackupManifest{}, backupFail("read backup manifest", err, path)
	}
	var manifest BackupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return BackupManifest{}, fmt.Errorf("parse backup manifest: %w", err)
	}
	return manifest, nil
}

func VerifyRestore(ctx context.Context, backupDir, artifactRoot string) (RestoreReport, error) {
	manifest, err := ReadBackupManifest(backupDir)
	if err != nil {
		return RestoreReport{}, err
	}

	db, err := openReadOnly(ctx, filepath.Join(backupDir, backupDatabaseFilename))
	if err != nil {
		return RestoreReport{}, backupFail("open restored database", err, filepath.Join(backupDir, backupDatabaseFilename))
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
			return RestoreReport{}, backupFail("checksum blob", err, absPath)
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

func errBackupDestinationConflict() error {
	return &Error{Code: ErrorCodeBackupDestinationConflict, Detail: "backup database already exists"}
}

func backupFail(prefix string, cause error, paths ...string) error {
	return &redactedError{prefix: prefix, cause: cause, paths: paths}
}

type redactedError struct {
	prefix string
	cause  error
	paths  []string
}

func (e *redactedError) Error() string {
	msg := e.prefix + ": " + e.cause.Error()
	for _, p := range e.paths {
		msg = strings.ReplaceAll(msg, p, filepath.Base(p))
	}
	return msg
}

func (e *redactedError) Unwrap() error { return e.cause }

func syncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func writeSynced(tmpPath, finalPath string, data []byte) error {
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, finalPath)
}
