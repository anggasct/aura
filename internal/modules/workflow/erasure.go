package workflow

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	legacyStepSessionOwner = "workflow"

	reconcileRunTree = `WITH RECURSIVE tree(id) AS (
		SELECT ?1 UNION ALL SELECT r.id FROM workflow_run r JOIN tree ON r.parent_run_id = tree.id)
		UPDATE workflow_run SET status = ?2, updated_at = ?3
		WHERE id IN (SELECT id FROM tree) AND status IN ('queued', 'running', 'suspended')`
)

type ErasureSessionPort interface {
	SessionIDs(ctx context.Context, ownerID string, runSessions bool) ([]string, error)
	FilterSessions(ctx context.Context, ownerID string, ids []string) ([]string, error)
	DeleteSessionsByID(ctx context.Context, ownerID string, ids []string) error
}

type ErasureDurablePort interface {
	RunStatesByKeys(ctx context.Context, handler string, keys []string) (map[string]string, error)
	DeleteHandlerRuns(ctx context.Context, handler string, keys []string) error
}

type ErasureEffectPort interface {
	DeleteApprovalsForSessions(ctx context.Context, sessionIDs []string) error
}

type Eraser struct {
	db       *sql.DB
	sessions ErasureSessionPort
	durable  ErasureDurablePort
	effects  ErasureEffectPort
}

func NewEraser(db *sql.DB, sessions ErasureSessionPort, durable ErasureDurablePort, effects ErasureEffectPort) (*Eraser, error) {
	if db == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow eraser requires a database")
	}
	if sessions == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow eraser requires a session port")
	}
	if durable == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow eraser requires a durable port")
	}
	if effects == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow eraser requires an effect port")
	}
	return &Eraser{db: db, sessions: sessions, durable: durable, effects: effects}, nil
}

func LiveWorkflowRuns(ctx context.Context, db *sql.DB, principalID string, sessions ErasureSessionPort, durable ErasureDurablePort) ([]string, error) {
	if db == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "live workflow runs requires a database")
	}
	if sessions == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "live workflow runs requires a session port")
	}
	if durable == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "live workflow runs requires a durable port")
	}
	var live []string
	if err := sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		var err error
		live, _, _, err = reconciledLiveRuns(txCtx, db, principalID, sessions, durable)
		return err
	}); err != nil {
		return nil, err
	}
	return live, nil
}

func (e *Eraser) ErasePrincipal(ctx context.Context, state *principal.ErasureState) error {
	if e == nil || e.db == nil {
		return codedError(ErrorCodeInvalidArgument, "workflow eraser requires a database")
	}
	if e.sessions == nil || e.durable == nil || e.effects == nil {
		return codedError(ErrorCodeInvalidArgument, "workflow eraser is incomplete")
	}
	if state == nil {
		return codedError(ErrorCodeInvalidArgument, "erasure state must not be nil")
	}
	scope := state.Scope()
	principalID := scope.PrincipalID
	q := sqlite.Conn(ctx, e.db)

	live, ownedSessions, held, err := reconciledLiveRuns(ctx, e.db, principalID, e.sessions, e.durable)
	if err != nil {
		return err
	}
	if err := state.Record(principal.ErasureLineLiveWorkflowRuns, len(live)); err != nil {
		return err
	}
	if scope.Apply && len(live) > 0 {
		return fault.New(
			principal.ErrorCodeErasureBlocked,
			fmt.Sprintf("runs %s are still running; run forget again", strings.Join(live, ", ")),
		)
	}

	var runCount int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_run WHERE principal_id = ?`, principalID).Scan(&runCount); err != nil {
		return sqlite.Classify(fmt.Errorf("count workflow runs: %w", err))
	}
	if err := state.Record(principal.ErasureLineWorkflowRuns, runCount); err != nil {
		return err
	}

	approvalCount, err := countHeldProposalApprovals(ctx, q, held.proposalIDs)
	if err != nil {
		return err
	}
	if err := state.Record(principal.ErasureLineWorkflowApprovals, approvalCount); err != nil {
		return err
	}

	legacy, err := existingLegacyStepSessions(ctx, q, e.sessions, principalID)
	if err != nil {
		return err
	}
	if err := state.Record(principal.ErasureLineLegacyStepSessions, len(legacy)); err != nil {
		return err
	}
	if !scope.Apply {
		return nil
	}

	if len(legacy) > 0 {
		if err := e.effects.DeleteApprovalsForSessions(ctx, legacy); err != nil {
			return err
		}
		if err := e.sessions.DeleteSessionsByID(ctx, legacyStepSessionOwner, legacy); err != nil {
			return err
		}
	}

	if len(ownedSessions) > 0 {
		encoded, err := json.Marshal(ownedSessions)
		if err != nil {
			return fmt.Errorf("encode owned sessions: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE workflow_run SET origin_session_id = NULL, reply_account = NULL, reply_channel_id = NULL
			WHERE principal_id IS NOT ?1 AND origin_session_id IN (SELECT value FROM json_each(?2))`,
			principalID, string(encoded),
		); err != nil {
			return sqlite.Classify(fmt.Errorf("clear workflow run origin metadata: %w", err))
		}
	}

	durableKeys, err := ownedRootDurableKeys(ctx, q, principalID)
	if err != nil {
		return err
	}
	if len(durableKeys) > 0 {
		if err := e.durable.DeleteHandlerRuns(ctx, HandlerName, durableKeys); err != nil {
			return err
		}
	}

	if _, err := q.ExecContext(ctx, `DELETE FROM workflow_run WHERE principal_id = ?`, principalID); err != nil {
		return sqlite.Classify(fmt.Errorf("delete workflow runs: %w", err))
	}

	if len(held.proposalIDs) > 0 {
		encoded, err := json.Marshal(held.proposalIDs)
		if err != nil {
			return fmt.Errorf("encode held proposal ids: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`DELETE FROM workflow_approval WHERE kind = 'proposal' AND proposal_id IN (SELECT value FROM json_each(?))`,
			string(encoded),
		); err != nil {
			return sqlite.Classify(fmt.Errorf("delete workflow proposal approvals: %w", err))
		}
	}

	for _, key := range held.definitions {
		if _, err := q.ExecContext(ctx,
			`DELETE FROM workflow_definition WHERE id = ?1 AND version = ?2
			AND NOT EXISTS (SELECT 1 FROM workflow_run WHERE definition_id = ?1 AND definition_version = ?2)`,
			key.id, key.version,
		); err != nil {
			return sqlite.Classify(fmt.Errorf("erase workflow definition: %w", err))
		}
	}
	return nil
}

type definitionKey struct {
	id      string
	version int
}

type heldProposalSet struct {
	proposalIDs []string
	definitions []definitionKey
}

type touchedRootRun struct {
	id         string
	durableKey string
	status     string
}

func allOwnedSessions(ctx context.Context, sessions ErasureSessionPort, principalID string) ([]string, error) {
	regular, err := sessions.SessionIDs(ctx, principalID, false)
	if err != nil {
		return nil, err
	}
	runSessions, err := sessions.SessionIDs(ctx, principalID, true)
	if err != nil {
		return nil, err
	}
	combined := slices.Concat(regular, runSessions)
	if len(combined) <= 1 {
		return combined, nil
	}
	slices.Sort(combined)
	return slices.Compact(combined), nil
}

func readHeldProposals(ctx context.Context, q sqlite.Querier, ownedSessions []string) (heldProposalSet, error) {
	if len(ownedSessions) == 0 {
		return heldProposalSet{}, nil
	}
	encodedSessions, err := json.Marshal(ownedSessions)
	if err != nil {
		return heldProposalSet{}, fmt.Errorf("encode owned sessions: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT id FROM workflow_proposal WHERE session_id IN (SELECT value FROM json_each(?))`,
		string(encodedSessions),
	)
	if err != nil {
		return heldProposalSet{}, sqlite.Classify(fmt.Errorf("read held proposals: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var proposalIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return heldProposalSet{}, sqlite.Classify(fmt.Errorf("scan held proposal: %w", err))
		}
		proposalIDs = append(proposalIDs, id)
	}
	if err := rows.Err(); err != nil {
		return heldProposalSet{}, sqlite.Classify(fmt.Errorf("read held proposals: %w", err))
	}
	if len(proposalIDs) == 0 {
		return heldProposalSet{}, nil
	}
	encodedProposals, err := json.Marshal(proposalIDs)
	if err != nil {
		return heldProposalSet{}, fmt.Errorf("encode held proposal ids: %w", err)
	}
	defRows, err := q.QueryContext(ctx,
		`SELECT definition_id, definition_version FROM workflow_approval WHERE kind = 'proposal' AND proposal_id IN (SELECT value FROM json_each(?))`,
		string(encodedProposals),
	)
	if err != nil {
		return heldProposalSet{}, sqlite.Classify(fmt.Errorf("read held proposal definitions: %w", err))
	}
	defer func() { _ = defRows.Close() }()
	var defs []definitionKey
	for defRows.Next() {
		var key definitionKey
		if err := defRows.Scan(&key.id, &key.version); err != nil {
			return heldProposalSet{}, sqlite.Classify(fmt.Errorf("scan held proposal definition: %w", err))
		}
		defs = append(defs, key)
	}
	if err := defRows.Err(); err != nil {
		return heldProposalSet{}, sqlite.Classify(fmt.Errorf("read held proposal definitions: %w", err))
	}
	return heldProposalSet{proposalIDs: proposalIDs, definitions: defs}, nil
}

func scanTouchedRootRuns(ctx context.Context, q sqlite.Querier, byID map[string]touchedRootRun, query string, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return sqlite.Classify(fmt.Errorf("read touched workflow runs: %w", err))
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var run touchedRootRun
		if err := rows.Scan(&run.id, &run.durableKey, &run.status); err != nil {
			return sqlite.Classify(fmt.Errorf("scan touched workflow run: %w", err))
		}
		byID[run.id] = run
	}
	if err := rows.Err(); err != nil {
		return sqlite.Classify(fmt.Errorf("read touched workflow runs: %w", err))
	}
	return nil
}

func readTouchedRootRuns(ctx context.Context, q sqlite.Querier, principalID string, defs []definitionKey) ([]touchedRootRun, error) {
	byID := map[string]touchedRootRun{}
	if err := scanTouchedRootRuns(ctx, q, byID,
		`SELECT id, COALESCE(durable_key, ''), status FROM workflow_run WHERE parent_run_id IS NULL AND principal_id = ?`,
		principalID,
	); err != nil {
		return nil, err
	}
	for _, key := range defs {
		if err := scanTouchedRootRuns(ctx, q, byID,
			`SELECT id, COALESCE(durable_key, ''), status FROM workflow_run WHERE parent_run_id IS NULL AND definition_id = ? AND definition_version = ?`,
			key.id, key.version,
		); err != nil {
			return nil, err
		}
	}
	touched := make([]touchedRootRun, 0, len(byID))
	for _, run := range byID {
		touched = append(touched, run)
	}
	slices.SortFunc(touched, func(a, b touchedRootRun) int {
		return cmp.Compare(a.id, b.id)
	})
	return touched, nil
}

func reconciledLiveRuns(ctx context.Context, db *sql.DB, principalID string, sessions ErasureSessionPort, durable ErasureDurablePort) (live, ownedSessions []string, held heldProposalSet, err error) {
	q := sqlite.Conn(ctx, db)
	ownedSessions, err = allOwnedSessions(ctx, sessions, principalID)
	if err != nil {
		return nil, nil, heldProposalSet{}, err
	}
	held, err = readHeldProposals(ctx, q, ownedSessions)
	if err != nil {
		return nil, nil, heldProposalSet{}, err
	}
	touched, err := readTouchedRootRuns(ctx, q, principalID, held.definitions)
	if err != nil {
		return nil, nil, heldProposalSet{}, err
	}
	var durableKeys []string
	for _, run := range touched {
		if run.durableKey != "" {
			durableKeys = append(durableKeys, run.durableKey)
		}
	}
	var durableStates map[string]string
	if len(durableKeys) > 0 {
		slices.Sort(durableKeys)
		durableKeys = slices.Compact(durableKeys)
		durableStates, err = durable.RunStatesByKeys(ctx, HandlerName, durableKeys)
		if err != nil {
			return nil, nil, heldProposalSet{}, err
		}
	}

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, run := range touched {
		durableState := ""
		if run.durableKey != "" {
			durableState = durableStates[run.durableKey]
		}
		if isActiveWorkflowStatus(run.status) && durableState != "running" {
			target := reconciledWorkflowStatus(durableState)
			if _, err := q.ExecContext(ctx, reconcileRunTree, run.id, target, stamp); err != nil {
				return nil, nil, heldProposalSet{}, sqlite.Classify(fmt.Errorf("reconcile workflow run: %w", err))
			}
		}
		if run.durableKey != "" && durableState == "running" {
			live = append(live, run.id)
		}
	}
	return live, ownedSessions, held, nil
}

func isActiveWorkflowStatus(status string) bool {
	return status == RunQueued || status == RunRunning || status == RunSuspended
}

func reconciledWorkflowStatus(durableState string) string {
	switch durableState {
	case RunCancelled, RunSucceeded:
		return durableState
	default:
		return RunFailed
	}
}

func countHeldProposalApprovals(ctx context.Context, q sqlite.Querier, proposalIDs []string) (int, error) {
	if len(proposalIDs) == 0 {
		return 0, nil
	}
	encoded, err := json.Marshal(proposalIDs)
	if err != nil {
		return 0, fmt.Errorf("encode held proposal ids: %w", err)
	}
	var count int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_approval WHERE kind = 'proposal' AND proposal_id IN (SELECT value FROM json_each(?))`,
		string(encoded),
	).Scan(&count); err != nil {
		return 0, sqlite.Classify(fmt.Errorf("count workflow approvals: %w", err))
	}
	return count, nil
}

func legacyStepSessionID(runID, stepID string) string {
	sum := sha256.Sum256([]byte(runID + "/" + stepID))
	return "wf-" + hex.EncodeToString(sum[:])[:24]
}

func existingLegacyStepSessions(ctx context.Context, q sqlite.Querier, sessions ErasureSessionPort, principalID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT s.run_id, s.step_id FROM workflow_step_run s JOIN workflow_run r ON r.id = s.run_id WHERE r.principal_id = ?`,
		principalID,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read workflow steps: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var candidates []string
	for rows.Next() {
		var runID, stepID string
		if err := rows.Scan(&runID, &stepID); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan workflow step: %w", err))
		}
		candidates = append(candidates, legacyStepSessionID(runID, stepID))
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read workflow steps: %w", err))
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return sessions.FilterSessions(ctx, legacyStepSessionOwner, candidates)
}

func ownedRootDurableKeys(ctx context.Context, q sqlite.Querier, principalID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT durable_key FROM workflow_run WHERE principal_id = ? AND parent_run_id IS NULL AND durable_key IS NOT NULL`,
		principalID,
	)
	if err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read owned durable keys: %w", err))
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, sqlite.Classify(fmt.Errorf("scan owned durable key: %w", err))
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlite.Classify(fmt.Errorf("read owned durable keys: %w", err))
	}
	return keys, nil
}
