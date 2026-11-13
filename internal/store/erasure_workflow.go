package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	workflowHandler = "workflow"

	legacyStepSessionOwner = "workflow"

	heldProposalIDs = `SELECT id FROM workflow_proposal WHERE session_id IN (SELECT id FROM session WHERE owner_id = ?1)`

	heldProposalApprovals = `kind = 'proposal' AND proposal_id IN (` + heldProposalIDs + `)`

	heldProposalDefinitions = `SELECT definition_id, definition_version FROM workflow_approval WHERE ` + heldProposalApprovals

	touchedRootRuns = `SELECT id FROM workflow_run WHERE parent_run_id IS NULL AND (
		principal_id = ?1 OR (definition_id, definition_version) IN (` + heldProposalDefinitions + `))`

	reconcileRunTree = `WITH RECURSIVE tree(id) AS (
		SELECT ?1 UNION ALL SELECT r.id FROM workflow_run r JOIN tree ON r.parent_run_id = tree.id)
		UPDATE workflow_run SET status = ?2, updated_at = ?3
		WHERE id IN (SELECT id FROM tree) AND status IN ('queued', 'running', 'suspended')`
)

type workflowErasureConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func LiveWorkflowRuns(ctx context.Context, db *sql.DB, principalID string) ([]string, error) {
	if db == nil {
		return nil, errNilArgument("db")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifyBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	live, err := reconciledLiveRuns(ctx, tx, principalID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, classifyBusy(err)
	}
	return live, nil
}

func reconciledLiveRuns(ctx context.Context, conn workflowErasureConn, principalID string) ([]string, error) {
	if err := reconcileStaleRuns(ctx, conn, principalID); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT r.id FROM workflow_run r
		JOIN durable_run d ON d.key = r.durable_key AND d.handler = ?2
		WHERE d.state = 'running' AND r.id IN (`+touchedRootRuns+`)
		ORDER BY r.id`, principalID, workflowHandler)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read live workflow runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var live []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan live workflow run: %w", err)
		}
		live = append(live, id)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read live workflow runs: %w", err))
	}
	return live, nil
}

type staleRun struct {
	id     string
	status string
}

func readStaleRuns(ctx context.Context, conn workflowErasureConn, principalID string) ([]staleRun, error) {
	rows, err := conn.QueryContext(ctx, `SELECT r.id, COALESCE(d.state, '') FROM workflow_run r
		LEFT JOIN durable_run d ON d.key = r.durable_key AND d.handler = ?2
		WHERE r.status IN ('queued', 'running', 'suspended') AND COALESCE(d.state, '') != 'running'
			AND r.id IN (`+touchedRootRuns+`)`, principalID, workflowHandler)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read stale workflow runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var stale []staleRun
	for rows.Next() {
		var run staleRun
		var durableState string
		if err := rows.Scan(&run.id, &durableState); err != nil {
			return nil, fmt.Errorf("scan stale workflow run: %w", err)
		}
		switch durableState {
		case "cancelled", "succeeded":
			run.status = durableState
		default:
			run.status = "failed"
		}
		stale = append(stale, run)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read stale workflow runs: %w", err))
	}
	return stale, nil
}

func reconcileStaleRuns(ctx context.Context, conn workflowErasureConn, principalID string) error {
	stale, err := readStaleRuns(ctx, conn, principalID)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, run := range stale {
		if _, err := conn.ExecContext(ctx, reconcileRunTree, run.id, run.status, stamp); err != nil {
			return classifyBusy(fmt.Errorf("reconcile workflow run: %w", err))
		}
	}
	return nil
}

func legacyStepSessionID(runID, stepID string) string {
	sum := sha256.Sum256([]byte(runID + "/" + stepID))
	return "wf-" + hex.EncodeToString(sum[:])[:24]
}

func legacyStepSessionCandidates(ctx context.Context, conn workflowErasureConn, principalID string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT s.run_id, s.step_id FROM workflow_step_run s
		JOIN workflow_run r ON r.id = s.run_id WHERE r.principal_id = ?`, principalID)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read workflow steps: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var candidates []string
	for rows.Next() {
		var runID, stepID string
		if err := rows.Scan(&runID, &stepID); err != nil {
			return nil, fmt.Errorf("scan workflow step: %w", err)
		}
		candidates = append(candidates, legacyStepSessionID(runID, stepID))
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read workflow steps: %w", err))
	}
	return candidates, nil
}

func existingLegacyStepSessions(ctx context.Context, conn workflowErasureConn, principalID string) ([]string, error) {
	candidates, err := legacyStepSessionCandidates(ctx, conn, principalID)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(candidates)
	if err != nil {
		return nil, fmt.Errorf("encode legacy step sessions: %w", err)
	}
	found, err := conn.QueryContext(ctx, `SELECT id FROM session WHERE owner_id = ? AND id IN (SELECT value FROM json_each(?))`, legacyStepSessionOwner, string(encoded))
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read legacy step sessions: %w", err))
	}
	defer func() { _ = found.Close() }()
	var ids []string
	for found.Next() {
		var id string
		if err := found.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan legacy step session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := found.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read legacy step sessions: %w", err))
	}
	return ids, nil
}

type definitionKey struct {
	id      string
	version int
}

func heldProposalDefinitionKeys(ctx context.Context, conn workflowErasureConn, principalID string) ([]definitionKey, error) {
	rows, err := conn.QueryContext(ctx, heldProposalDefinitions, principalID)
	if err != nil {
		return nil, classifyBusy(fmt.Errorf("read held proposal definitions: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var keys []definitionKey
	for rows.Next() {
		var key definitionKey
		if err := rows.Scan(&key.id, &key.version); err != nil {
			return nil, fmt.Errorf("scan held proposal definition: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyBusy(fmt.Errorf("read held proposal definitions: %w", err))
	}
	return keys, nil
}

func eraseWorkflowData(ctx context.Context, conn workflowErasureConn, principalID string, apply bool, counts *ErasureCounts) error {
	live, err := reconciledLiveRuns(ctx, conn, principalID)
	if err != nil {
		return err
	}
	counts.LiveWorkflowRuns = len(live)
	if apply && len(live) > 0 {
		return &Error{
			Code:   ErrorCodeErasureBlocked,
			Detail: fmt.Sprintf("runs %s are still running; run forget again", strings.Join(live, ", ")),
		}
	}
	for _, count := range []struct {
		field *int
		query string
	}{
		{&counts.WorkflowRuns, `SELECT COUNT(*) FROM workflow_run WHERE principal_id = ?1`},
		{&counts.WorkflowApprovals, `SELECT COUNT(*) FROM workflow_approval WHERE ` + heldProposalApprovals},
	} {
		if err := conn.QueryRowContext(ctx, count.query, principalID).Scan(count.field); err != nil {
			return classifyBusy(fmt.Errorf("count workflow data: %w", err))
		}
	}
	legacy, err := existingLegacyStepSessions(ctx, conn, principalID)
	if err != nil {
		return err
	}
	counts.LegacyStepSessions = len(legacy)
	if !apply {
		return nil
	}
	keys, err := heldProposalDefinitionKeys(ctx, conn, principalID)
	if err != nil {
		return err
	}
	if err := eraseLegacyStepSessions(ctx, conn, legacy); err != nil {
		return err
	}
	for _, statement := range []string{
		`UPDATE workflow_run SET origin_session_id = NULL, reply_account = NULL, reply_channel_id = NULL
			WHERE principal_id IS NOT ?1 AND origin_session_id IN (SELECT id FROM session WHERE owner_id = ?1)`,
		`DELETE FROM durable_run WHERE handler = '` + workflowHandler + `' AND key IN (
			SELECT durable_key FROM workflow_run WHERE principal_id = ?1 AND parent_run_id IS NULL AND durable_key IS NOT NULL)`,
		`DELETE FROM workflow_run WHERE principal_id = ?1`,
		`DELETE FROM workflow_approval WHERE ` + heldProposalApprovals,
	} {
		if _, err := conn.ExecContext(ctx, statement, principalID); err != nil {
			return classifyBusy(fmt.Errorf("erase workflow data: %w", err))
		}
	}
	for _, key := range keys {
		if _, err := conn.ExecContext(ctx, `DELETE FROM workflow_definition WHERE id = ?1 AND version = ?2
			AND NOT EXISTS (SELECT 1 FROM workflow_run WHERE definition_id = ?1 AND definition_version = ?2)`, key.id, key.version); err != nil {
			return classifyBusy(fmt.Errorf("erase workflow definition: %w", err))
		}
	}
	return nil
}

func eraseLegacyStepSessions(ctx context.Context, conn workflowErasureConn, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode legacy step sessions: %w", err)
	}
	for _, statement := range []string{
		`DELETE FROM effect_approval WHERE intent_id IN (SELECT id FROM effect_intent WHERE session_id IN (SELECT value FROM json_each(?1)))`,
		`DELETE FROM session WHERE owner_id = '` + legacyStepSessionOwner + `' AND id IN (SELECT value FROM json_each(?1))`,
	} {
		if _, err := conn.ExecContext(ctx, statement, string(encoded)); err != nil {
			return classifyBusy(fmt.Errorf("erase legacy step sessions: %w", err))
		}
	}
	return nil
}
