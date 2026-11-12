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
	EmailEffects    int
	Blobs           int
}

const (
	emailEffectMatch = `substr(?1, 1, 6) = 'email:' AND json_valid(request_json) AND (
		(provider = 'email' AND lower(json_extract(request_json, '$.to')) = substr(?1, 7))
		OR (provider = 'builtin-tools' AND operation = 'email_send' AND lower(json_extract(request_json, '$.arguments.to')) = substr(?1, 7)))`

	emailEffectIDs = `SELECT id FROM effect_intent WHERE ` + emailEffectMatch

	ownedEffectIDs = `SELECT id FROM effect_intent WHERE session_id IN (SELECT id FROM session WHERE owner_id = ?1)`

	turnRunSessionID = `CASE WHEN json_valid(CAST(durable_run.payload AS TEXT)) THEN json_extract(CAST(durable_run.payload AS TEXT), '$.session_id') END`

	orphanedTurnRuns = `DELETE FROM durable_run WHERE handler = 'turn' AND ` + turnRunSessionID + ` IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM session WHERE session.id = ` + turnRunSessionID + `)`
)

type erasureTarget struct {
	field *int
	count string
	erase []string
	grant bool
}

func (c *ErasureCounts) targets() []erasureTarget {
	return []erasureTarget{
		{field: &c.EmailEffects, count: `SELECT COUNT(*) FROM effect_intent WHERE ` + emailEffectMatch, erase: []string{
			`DELETE FROM effect_approval WHERE intent_id IN (` + emailEffectIDs + `)`,
			`DELETE FROM effect_intent WHERE id IN (` + emailEffectIDs + `)`,
		}},
		{field: &c.MemoryDocuments, count: `SELECT COUNT(*) FROM memory_document WHERE owner_id = ?`, erase: []string{`DELETE FROM memory_document WHERE owner_id = ?`}},
		{field: &c.Sessions, count: `SELECT COUNT(*) FROM session WHERE owner_id = ?`, erase: []string{
			`DELETE FROM effect_approval WHERE intent_id IN (` + ownedEffectIDs + `)`,
			`DELETE FROM session WHERE owner_id = ?`,
			orphanedTurnRuns,
		}},
		{field: &c.Grants, count: `SELECT COUNT(*) FROM principal_grant WHERE principal_id = ?`, erase: []string{`DELETE FROM principal_grant WHERE principal_id = ?`}, grant: true},
		{field: &c.StandingGrants, count: `SELECT COUNT(*) FROM standing_grant WHERE principal_id = ?`, erase: []string{`DELETE FROM standing_grant WHERE principal_id = ?`}},
		{field: &c.PairingRequests, count: `SELECT COUNT(*) FROM pairing_request WHERE principal_id = ?`, erase: []string{`DELETE FROM pairing_request WHERE principal_id = ?`}},
		{field: &c.UsageLinks, count: `SELECT COUNT(*) FROM usage_reservation WHERE principal_id = ?`, erase: []string{`UPDATE usage_reservation SET principal_id = NULL WHERE principal_id = ?`}},
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
		for _, statement := range target.erase {
			if _, err := tx.ExecContext(ctx, statement, principalID); err != nil {
				return counts, classifyBusy(fmt.Errorf("erase principal data: %w", err))
			}
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
