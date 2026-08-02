package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// ReconciliationReport is the spec name for the findings produced by
// Reconciler.Inspect (the underlying scan is Reconcile).
type ReconciliationReport = ReconcileReport

// CollectionReport reports what an unreferenced-blob sweep deleted.
type CollectionReport struct {
	DeletedBlobs int
	FreedBytes   int64
}

// Reconciler is the spec contract for reconciliation and garbage collection.
type Reconciler interface {
	Inspect(ctx context.Context) (ReconciliationReport, error)
	Collect(ctx context.Context, before time.Time) (CollectionReport, error)
}

type sqliteReconciler struct {
	db   *sql.DB
	root string
}

func NewReconciler(db *sql.DB, root string) Reconciler {
	return &sqliteReconciler{db: db, root: root}
}

func (r *sqliteReconciler) Inspect(ctx context.Context) (ReconciliationReport, error) {
	return Reconcile(ctx, r.db, r.root)
}

func (r *sqliteReconciler) Collect(ctx context.Context, before time.Time) (CollectionReport, error) {
	return Collect(ctx, r.db, r.root, before)
}

// Collect deletes blob files and their rows for blobs that have no artifact
// reference and were created before the grace cutoff. The delete is
// conditional and transactional: the NOT EXISTS reference check runs in the
// same statement as the row delete, so a reference linked after the candidate
// scan still protects the blob. Files are removed only when the row
// delete actually matched; a file that cannot be removed aborts the sweep
// with its row intact, so a failed sweep never deletes ownership data.
func Collect(ctx context.Context, db *sql.DB, root string, before time.Time) (CollectionReport, error) {
	rows, err := db.QueryContext(ctx, `SELECT digest, size_bytes, relative_path FROM blob b
		WHERE NOT EXISTS (SELECT 1 FROM artifact_ref r WHERE r.blob_digest = b.digest)
		AND b.created_at < ?`, formatTime(before))
	if err != nil {
		return CollectionReport{}, classifyBusy(err)
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
			return CollectionReport{}, fmt.Errorf("scan collectible blob: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Close(); err != nil {
		return CollectionReport{}, fmt.Errorf("close collectible blob scan: %w", err)
	}

	var report CollectionReport
	if len(candidates) == 0 {
		return report, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return CollectionReport{}, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, c := range candidates {
		res, err := tx.ExecContext(ctx, `DELETE FROM blob WHERE digest = ? AND NOT EXISTS
			(SELECT 1 FROM artifact_ref r WHERE r.blob_digest = ?)`, c.digest, c.digest)
		if err != nil {
			return CollectionReport{}, classifyBusy(err)
		}
		matched, err := res.RowsAffected()
		if err != nil {
			return CollectionReport{}, fmt.Errorf("rows affected for blob %s: %w", c.digest, err)
		}
		if matched == 0 {
			continue
		}
		absPath, err := resolveRootedPath(root, c.relativePath)
		if err != nil {
			return CollectionReport{}, err
		}
		if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
			return CollectionReport{}, fmt.Errorf("remove unreferenced blob %s: %w", c.digest, err)
		}
		report.DeletedBlobs++
		report.FreedBytes += c.size
	}
	if err := tx.Commit(); err != nil {
		return CollectionReport{}, classifyBusy(err)
	}
	return report, nil
}
