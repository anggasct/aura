package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ErasureOptions struct {
	KeepGrants   bool
	ArtifactRoot string
}

type ErasureCounts struct {
	Sessions        int
	MemoryDocuments int
	Grants          int
	StandingGrants  int
	PairingRequests int
	UsageLinks      int
	Blobs           int
}

type erasureTarget struct {
	field *int
	count string
	erase string
	grant bool
}

func (c *ErasureCounts) targets() []erasureTarget {
	return []erasureTarget{
		{field: &c.MemoryDocuments, count: `SELECT COUNT(*) FROM memory_document WHERE owner_id = ?`, erase: `DELETE FROM memory_document WHERE owner_id = ?`},
		{field: &c.Sessions, count: `SELECT COUNT(*) FROM session WHERE owner_id = ?`, erase: `DELETE FROM session WHERE owner_id = ?`},
		{field: &c.Grants, count: `SELECT COUNT(*) FROM principal_grant WHERE principal_id = ?`, erase: `DELETE FROM principal_grant WHERE principal_id = ?`, grant: true},
		{field: &c.StandingGrants, count: `SELECT COUNT(*) FROM standing_grant WHERE principal_id = ?`, erase: `DELETE FROM standing_grant WHERE principal_id = ?`},
		{field: &c.PairingRequests, count: `SELECT COUNT(*) FROM pairing_request WHERE principal_id = ?`, erase: `DELETE FROM pairing_request WHERE principal_id = ?`},
		{field: &c.UsageLinks, count: `SELECT COUNT(*) FROM usage_reservation WHERE principal_id = ?`, erase: `UPDATE usage_reservation SET principal_id = NULL WHERE principal_id = ?`},
	}
}

func ErasePrincipal(ctx context.Context, db *sql.DB, principalID string, apply bool, opts ErasureOptions) (ErasureCounts, error) {
	var counts ErasureCounts
	if db == nil {
		return counts, errors.New("erase principal: database must not be nil")
	}
	if principalID == "" || !strings.Contains(principalID, ":") {
		return counts, fmt.Errorf("erase principal: %q is not a channel principal", principalID)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return counts, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, target := range counts.targets() {
		if target.grant && opts.KeepGrants {
			continue
		}
		if err := tx.QueryRowContext(ctx, target.count, principalID).Scan(target.field); err != nil {
			return counts, classifyBusy(fmt.Errorf("count principal data: %w", err))
		}
		if !apply || *target.field == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, target.erase, principalID); err != nil {
			return counts, classifyBusy(fmt.Errorf("erase principal data: %w", err))
		}
	}
	if !apply {
		return counts, nil
	}
	if err := tx.Commit(); err != nil {
		return counts, classifyBusy(err)
	}
	if opts.ArtifactRoot != "" && counts.Sessions > 0 {
		report, err := Collect(ctx, db, opts.ArtifactRoot, time.Now().UTC().Add(time.Second))
		if err != nil {
			return counts, fmt.Errorf("collect erased blobs: %w", err)
		}
		counts.Blobs = report.DeletedBlobs
	}
	return counts, nil
}
