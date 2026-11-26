package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/storage"
)

type ErasureOptions struct {
	KeepGrants   bool
	ArtifactRoot string
}

type ErasureCounts struct {
	Sessions        int
	RunSessions     int
	MemoryDocuments int
	Grants          int
	StandingGrants  int
	PairingRequests int
	UsageLinks      int
	EmailEffects    int
	Blobs           int

	WorkflowRuns       int
	WorkflowApprovals  int
	LegacyStepSessions int
	LiveWorkflowRuns   int
}

const (
	emailEffectMatch = `substr(?1, 1, 6) = 'email:' AND json_valid(request_json) AND (
		(provider = 'email' AND lower(json_extract(request_json, '$.to')) = substr(?1, 7))
		OR (provider = 'builtin-tools' AND operation = 'email_send' AND lower(json_extract(request_json, '$.arguments.to')) = substr(?1, 7)))`

	emailEffectIDs = `SELECT id FROM effect_intent WHERE ` + emailEffectMatch

	ownedEffectIDs = `SELECT id FROM effect_intent WHERE session_id IN (SELECT id FROM session WHERE owner_id = ?1 AND id NOT GLOB 'wf-*')`

	runSessionEffectIDs = `SELECT id FROM effect_intent WHERE session_id IN (SELECT id FROM session WHERE owner_id = ?1 AND id GLOB 'wf-*')`

	turnRunSessionID = `CASE WHEN json_valid(CAST(durable_run.payload AS TEXT)) THEN json_extract(CAST(durable_run.payload AS TEXT), '$.session_id') END`

	orphanedTurnRun = `durable_run.handler = 'turn' AND ` + turnRunSessionID + ` IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM runtime_event WHERE runtime_event.session_id = ` + turnRunSessionID + `
			AND runtime_event.turn_id = durable_run.key AND runtime_event.kind = 'turn.accepted')`

	orphanedTurnRuns = `DELETE FROM durable_run WHERE ` + orphanedTurnRun
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
		{field: &c.Sessions, count: `SELECT COUNT(*) FROM session WHERE owner_id = ? AND id NOT GLOB 'wf-*'`, erase: []string{
			`DELETE FROM effect_approval WHERE intent_id IN (` + ownedEffectIDs + `)`,
			`DELETE FROM session WHERE owner_id = ? AND id NOT GLOB 'wf-*'`,
			orphanedTurnRuns,
		}},
		{field: &c.RunSessions, count: `SELECT COUNT(*) FROM session WHERE owner_id = ? AND id GLOB 'wf-*'`, erase: []string{
			`DELETE FROM effect_approval WHERE intent_id IN (` + runSessionEffectIDs + `)`,
			`DELETE FROM session WHERE owner_id = ? AND id GLOB 'wf-*'`,
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
		return counts, classify(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := eraseWorkflowData(ctx, tx, principalID, apply, &counts); err != nil {
		return counts, err
	}
	for _, target := range counts.targets() {
		if target.grant && opts.KeepGrants {
			continue
		}
		if err := tx.QueryRowContext(ctx, target.count, principalID).Scan(target.field); err != nil {
			return counts, classify(fmt.Errorf("count principal data: %w", err))
		}
		if !apply || *target.field == 0 {
			continue
		}
		for _, statement := range target.erase {
			if _, err := tx.ExecContext(ctx, statement, principalID); err != nil {
				return counts, classify(fmt.Errorf("erase principal data: %w", err))
			}
		}
	}
	if !apply {
		return counts, nil
	}
	if err := tx.Commit(); err != nil {
		return counts, classify(err)
	}
	if opts.ArtifactRoot != "" && (counts.Sessions > 0 || counts.RunSessions > 0 || counts.LegacyStepSessions > 0) {
		report, err := storage.Collect(ctx, db, opts.ArtifactRoot, time.Now().UTC().Add(time.Second))
		if err != nil {
			return counts, fmt.Errorf("collect erased blobs: %w", err)
		}
		counts.Blobs = report.DeletedBlobs
	}
	return counts, nil
}
