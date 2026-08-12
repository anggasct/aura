package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

type ReconciliationReport = ReconcileReport

type CollectionReport struct {
	DeletedBlobs int
	FreedBytes   int64
}

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
	candidates, err := collectionCandidates(ctx, db, before)
	if err != nil {
		return CollectionReport{}, err
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

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM blob WHERE digest = ? AND NOT EXISTS
		(SELECT 1 FROM artifact_ref r WHERE r.blob_digest = ?)`)
	if err != nil {
		return CollectionReport{}, classifyBusy(err)
	}
	defer func() { _ = stmt.Close() }()

	for _, c := range candidates {
		res, err := stmt.ExecContext(ctx, c.digest, c.digest)
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

type collectionCandidate struct {
	digest       string
	size         int64
	relativePath string
}

func collectionCandidates(ctx context.Context, db *sql.DB, before time.Time) ([]collectionCandidate, error) {
	rows, err := db.QueryContext(ctx, `SELECT digest, size_bytes, relative_path FROM blob b
		WHERE NOT EXISTS (SELECT 1 FROM artifact_ref r WHERE r.blob_digest = b.digest)
		AND b.created_at < ?`, formatTime(before))
	if err != nil {
		return nil, classifyBusy(err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []collectionCandidate
	for rows.Next() {
		var c collectionCandidate
		if err := rows.Scan(&c.digest, &c.size, &c.relativePath); err != nil {
			return nil, fmt.Errorf("scan collectible blob: %w", err)
		}
		candidates = append(candidates, c)
	}
	// Without this a query that fails mid-iteration is indistinguishable from
	// an empty result, and the sweep would report success having seen nothing.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan collectible blobs: %w", err)
	}
	return candidates, nil
}
