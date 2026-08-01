package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// CollectReport reports what an unreferenced-blob sweep deleted.
type CollectReport struct {
	DeletedBlobs int
	FreedBytes   int64
}

// Reconciler is the spec contract for reconciliation and garbage collection.
type Reconciler interface {
	Inspect(ctx context.Context) (ReconcileReport, error)
	Collect(ctx context.Context, before time.Time) (CollectReport, error)
}

type sqliteReconciler struct {
	db   *sql.DB
	root string
}

func NewReconciler(db *sql.DB, root string) Reconciler {
	return &sqliteReconciler{db: db, root: root}
}

func (r *sqliteReconciler) Inspect(ctx context.Context) (ReconcileReport, error) {
	return Reconcile(ctx, r.db, r.root)
}

func (r *sqliteReconciler) Collect(ctx context.Context, before time.Time) (CollectReport, error) {
	return Collect(ctx, r.db, r.root, before)
}

// Collect deletes blob files and their rows for blobs that have no artifact
// reference and were created before the grace cutoff. Only blobs older than
// the grace window are eligible, so a fresh unreferenced blob is never
// removed. Files are removed before rows; a file that cannot be removed
// aborts the sweep with its row intact, so a failed sweep never deletes
// ownership data.
func Collect(ctx context.Context, db *sql.DB, root string, before time.Time) (CollectReport, error) {
	rows, err := db.QueryContext(ctx, `SELECT digest, size_bytes, relative_path FROM blob b
		WHERE NOT EXISTS (SELECT 1 FROM artifact_ref r WHERE r.blob_digest = b.digest)
		AND b.created_at < ?`, formatTime(before))
	if err != nil {
		return CollectReport{}, fmt.Errorf("list collectible blobs: %w", err)
	}
	type candidate struct {
		digest       string
		size         int64
		relativePath string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.digest, &c.size, &c.relativePath); err != nil {
			_ = rows.Close()
			return CollectReport{}, fmt.Errorf("scan collectible blob: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Close(); err != nil {
		return CollectReport{}, fmt.Errorf("close collectible blob scan: %w", err)
	}

	var report CollectReport
	for _, c := range candidates {
		absPath, err := resolveRootedPath(root, c.relativePath)
		if err != nil {
			return CollectReport{}, err
		}
		if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
			return CollectReport{}, fmt.Errorf("remove unreferenced blob %s: %w", c.digest, err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM blob WHERE digest = ?`, c.digest); err != nil {
			return CollectReport{}, fmt.Errorf("delete unreferenced blob %s: %w", c.digest, err)
		}
		report.DeletedBlobs++
		report.FreedBytes += c.size
	}
	return report, nil
}
