package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ReconcileReport lists storage inconsistencies. Reconcile never deletes or
// modifies data; it only reports findings for an operator or health check.
type ReconcileReport struct {
	MissingBlobs []string // digests with a blob row but no file on disk
	OrphanFiles  []string // paths on disk with no matching blob row
}

// Reconcile compares the blob table against the blob storage tree rooted at
// root, detecting rows whose file is missing and files with no owning row.
func Reconcile(ctx context.Context, db *sql.DB, root string) (ReconcileReport, error) {
	rows, err := db.QueryContext(ctx, `SELECT digest, relative_path FROM blob`)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("list blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	knownPaths := make(map[string]struct{})
	var report ReconcileReport
	for rows.Next() {
		var digest, relPath string
		if err := rows.Scan(&digest, &relPath); err != nil {
			return ReconcileReport{}, fmt.Errorf("scan blob row: %w", err)
		}
		knownPaths[filepath.ToSlash(relPath)] = struct{}{}
		absPath := filepath.Join(root, filepath.FromSlash(relPath))
		if _, err := os.Stat(absPath); err != nil {
			if os.IsNotExist(err) {
				report.MissingBlobs = append(report.MissingBlobs, digest)
				continue
			}
			return ReconcileReport{}, fmt.Errorf("stat blob %s: %w", digest, err)
		}
	}
	if err := rows.Err(); err != nil {
		return ReconcileReport{}, fmt.Errorf("list blobs: %w", err)
	}

	blobRoot := filepath.Join(root, "blobs")
	err = filepath.WalkDir(blobRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := knownPaths[rel]; !ok {
			report.OrphanFiles = append(report.OrphanFiles, rel)
		}
		return nil
	})
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("walk blob storage: %w", err)
	}

	return report, nil
}
