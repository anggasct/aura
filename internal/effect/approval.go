package effect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/store"
)

const (
	ApprovalActionMarkSucceeded ApprovalAction = "mark_succeeded"
	ApprovalActionMarkFailed    ApprovalAction = "mark_failed"
	ApprovalActionRetry         ApprovalAction = "retry"

	DefaultApprovalTTL   = 5 * time.Minute
	maxApprovalTTL       = 24 * time.Hour
	maxApprovalReasonLen = 1024
	retryInvocationID    = "effect-retry"
)

type ApprovalAction string

func (a ApprovalAction) valid() bool {
	switch a {
	case ApprovalActionMarkSucceeded, ApprovalActionMarkFailed, ApprovalActionRetry:
		return true
	default:
		return false
	}
}

type ApprovalRequest struct {
	IntentID  string
	Action    ApprovalAction
	Reason    string
	ExpiresIn time.Duration
}

type Approval struct {
	ID            string
	IntentID      string
	Action        ApprovalAction
	RequestDigest string
	OwnerID       string
	Reason        string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	Token         string
}

func (j *Journal) Approve(ctx context.Context, req *ApprovalRequest) (*Approval, error) {
	if req == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: approval request must not be nil", nil)
	}
	request := *req
	if request.ExpiresIn == 0 {
		request.ExpiresIn = DefaultApprovalTTL
	}
	if err := validateApprovalRequest(&request); err != nil {
		return nil, err
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("effect: begin approval transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	intent, err := scanIntent(tx.QueryRowContext(ctx, selectIntentByIDSQL, request.IntentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", request.IntentID), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: read intent %s: %w", request.IntentID, err)
	}
	if intent.State != StateUnknown {
		return nil, codedError(ErrorCodeTransitionInvalid,
			fmt.Sprintf("effect: intent %s is %s, approval requires unknown", request.IntentID, intent.State), nil)
	}

	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM session WHERE id = ?`, intent.SessionID).Scan(&ownerID); err != nil {
		return nil, fmt.Errorf("effect: read owner for intent %s: %w", req.IntentID, err)
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := j.now().UTC()
	expiresAt := now.Add(request.ExpiresIn)
	approvalID, err := randomID()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO effect_approval (
		    id, intent_id, action, request_digest, owner_id, reason, token_hash,
		    issued_at, expires_at, consumed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		approvalID, intent.ID, string(request.Action), intent.RequestDigest, ownerID, request.Reason,
		hashToken(token), fmtTime(now), fmtTime(expiresAt),
	)
	if err != nil {
		return nil, fmt.Errorf("effect: insert approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("effect: commit approval: %w", err)
	}
	j.observe(ctx, intent, "approval_issued")

	return &Approval{
		ID:            approvalID,
		IntentID:      intent.ID,
		Action:        request.Action,
		RequestDigest: intent.RequestDigest,
		OwnerID:       ownerID,
		Reason:        request.Reason,
		IssuedAt:      now,
		ExpiresAt:     expiresAt,
		Token:         token,
	}, nil
}

func (j *Journal) MarkWithApproval(ctx context.Context, id string, succeeded bool, reason, token string) (*Intent, error) {
	action := ApprovalActionMarkFailed
	if succeeded {
		action = ApprovalActionMarkSucceeded
	}
	return j.consumeApproval(ctx, id, action, reason, token)
}

func (j *Journal) RetryWithApproval(ctx context.Context, id, token string) (*Intent, error) {
	return j.consumeApproval(ctx, id, ApprovalActionRetry, "", token)
}

type approvalRecord struct {
	ID            string
	IntentID      string
	Action        ApprovalAction
	RequestDigest string
	OwnerID       string
	Reason        string
	ExpiresAt     time.Time
	ConsumedAt    *time.Time
}

func (j *Journal) consumeApproval(ctx context.Context, id string, action ApprovalAction, reason, token string) (*Intent, error) {
	if id == "" || token == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: intent ID and approval token are required", nil)
	}
	if action != ApprovalActionRetry && strings.TrimSpace(reason) == "" {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: resolution reason must not be empty", nil)
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("effect: begin approved operation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	approval, ownerID, err := readApproval(ctx, tx, hashToken(token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeApprovalInvalid, "effect: approval token is invalid", nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: read approval: %w", err)
	}
	if approval.ConsumedAt != nil {
		return nil, codedError(ErrorCodeApprovalConsumed, "effect: approval token was already consumed", nil)
	}
	intent, err := scanIntent(tx.QueryRowContext(ctx, selectIntentByIDSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", id), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("effect: read intent %s: %w", id, err)
	}
	if approval.IntentID != intent.ID || approval.Action != action || approval.RequestDigest != intent.RequestDigest || approval.OwnerID != ownerID {
		return nil, codedError(ErrorCodeApprovalInvalid, "effect: approval token binding does not match intent", nil)
	}
	now := j.now().UTC()
	if !now.Before(approval.ExpiresAt) {
		return nil, codedError(ErrorCodeApprovalExpired, "effect: approval token has expired", nil)
	}
	if action != ApprovalActionRetry && approval.Reason != reason {
		return nil, codedError(ErrorCodeApprovalInvalid, "effect: approval reason does not match", nil)
	}
	if intent.State != StateUnknown {
		return nil, codedError(ErrorCodeTransitionInvalid,
			fmt.Sprintf("effect: intent %s is %s, approval requires unknown", id, intent.State), nil)
	}

	var resolved *Intent
	if action == ApprovalActionRetry {
		resolved, err = createApprovedRetry(ctx, tx, intent, ownerID, now)
	} else {
		state := StateFailed
		safeErrorCode := "effect_owner_resolved_failed"
		if action == ApprovalActionMarkSucceeded {
			state = StateSucceeded
			safeErrorCode = ""
		}
		result, updateErr := tx.ExecContext(ctx, `
			UPDATE effect_intent
			SET state = ?, updated_at = ?, finished_at = ?, reconciled_at = ?, safe_error_code = ?
			WHERE id = ? AND state = ?`,
			string(state), fmtTime(now), fmtTime(now), fmtTime(now), nullableString(safeErrorCode), id, string(StateUnknown),
		)
		err = updateErr
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				err = codedError(ErrorCodeTransitionInvalid, "effect: intent changed before approved resolution", nil)
			}
		}
		if err == nil {
			resolved = &Intent{ID: id, State: state}
		}
	}
	if err != nil {
		return nil, err
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE effect_approval SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`,
		fmtTime(now), approval.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("effect: consume approval: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("effect: read approval consumption: %w", err)
	}
	if affected != 1 {
		return nil, codedError(ErrorCodeApprovalConsumed, "effect: approval token was consumed concurrently", nil)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("effect: commit approved operation: %w", err)
	}

	if action == ApprovalActionRetry {
		j.observe(ctx, resolved, "retry")
		return resolved, nil
	}
	resolved, err = j.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	j.observe(ctx, resolved, "owner_resolution")
	return resolved, nil
}

func readApproval(ctx context.Context, q queryer, tokenHash string) (*approvalRecord, string, error) {
	var record approvalRecord
	var action, expiresAt string
	var consumedAt sql.NullString
	var ownerID string
	err := q.QueryRowContext(ctx, `
		SELECT a.id, a.intent_id, a.action, a.request_digest, a.owner_id, a.reason,
		       a.expires_at, a.consumed_at, s.owner_id
		FROM effect_approval a
		JOIN effect_intent i ON i.id = a.intent_id
		JOIN session s ON s.id = i.session_id
		WHERE a.token_hash = ?`, tokenHash).Scan(
		&record.ID, &record.IntentID, &action, &record.RequestDigest, &record.OwnerID, &record.Reason,
		&expiresAt, &consumedAt, &ownerID,
	)
	if err != nil {
		return nil, "", err
	}
	record.Action = ApprovalAction(action)
	record.ExpiresAt, err = parseTime(expiresAt, "approval expires_at")
	if err != nil {
		return nil, "", err
	}
	if consumedAt.Valid {
		consumed, parseErr := parseTime(consumedAt.String, "approval consumed_at")
		if parseErr != nil {
			return nil, "", parseErr
		}
		record.ConsumedAt = &consumed
	}
	return &record, ownerID, nil
}

func createApprovedRetry(ctx context.Context, tx *sql.Tx, intent *Intent, ownerID string, now time.Time) (*Intent, error) {
	normalizedRequest, err := normalizeRequest(intent.RequestJSON)
	if err != nil {
		return nil, err
	}
	requestDigest := digestRequest(normalizedRequest)
	newID, err := randomID()
	if err != nil {
		return nil, err
	}
	newKey, err := randomID()
	if err != nil {
		return nil, err
	}
	var maxSequence sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(sequence) FROM runtime_event WHERE session_id = ?`, intent.SessionID,
	).Scan(&maxSequence); err != nil {
		return nil, fmt.Errorf("effect: read retry event sequence: %w", err)
	}
	sequence := int64(1)
	if maxSequence.Valid {
		if maxSequence.Int64 == math.MaxInt64 {
			return nil, errors.New("effect: retry event sequence exhausted")
		}
		sequence = maxSequence.Int64 + 1
	}
	payload, err := json.Marshal(toolRequestedPayload{
		EffectIntentID: newID,
		ToolCallID:     intent.ToolCallID,
		Provider:       intent.Provider,
		Operation:      intent.Operation,
		Classification: intent.Classification,
		IdempotencyKey: newKey,
		RequestDigest:  requestDigest,
		RetryOf:        intent.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("effect: marshal retry payload: %w", err)
	}
	eventID, err := randomID()
	if err != nil {
		return nil, err
	}
	if err := store.AppendEventTx(ctx, tx, &store.RuntimeEvent{
		ID:            eventID,
		SessionID:     intent.SessionID,
		Sequence:      uint64(sequence),
		TurnID:        intent.TurnID,
		InvocationID:  retryInvocationID,
		Branch:        "",
		Author:        ownerID,
		Kind:          EventKindToolRequested,
		SchemaVersion: toolRequestedSchemaVersion,
		Payload:       payload,
		CreatedAt:     now,
	}); err != nil {
		return nil, fmt.Errorf("effect: append retry event: %w", err)
	}
	_, err = tx.ExecContext(ctx, insertIntentSQLWithRetry,
		newID, intent.SessionID, intent.TurnID, intent.ToolCallID, newKey, intent.Provider, intent.Operation,
		string(intent.Classification), string(StatePrepared), requestDigest, string(normalizedRequest), intent.ID,
		fmtTime(now), fmtTime(now),
	)
	if err != nil {
		return nil, fmt.Errorf("effect: insert retry intent: %w", err)
	}
	return &Intent{
		ID:             newID,
		SessionID:      intent.SessionID,
		TurnID:         intent.TurnID,
		ToolCallID:     intent.ToolCallID,
		IdempotencyKey: newKey,
		Provider:       intent.Provider,
		Operation:      intent.Operation,
		Classification: intent.Classification,
		State:          StatePrepared,
		RequestDigest:  requestDigest,
		RequestJSON:    normalizedRequest,
		RetryOf:        intent.ID,
		PreparedAt:     now,
		UpdatedAt:      now,
	}, nil
}

func validateApprovalRequest(req *ApprovalRequest) error {
	var problems []error
	if req.IntentID == "" {
		problems = append(problems, errors.New("intent_id must not be empty"))
	}
	if !req.Action.valid() {
		problems = append(problems, errors.New("action is invalid"))
	}
	if strings.TrimSpace(req.Reason) == "" {
		problems = append(problems, errors.New("reason must not be empty"))
	}
	if len([]byte(req.Reason)) > maxApprovalReasonLen {
		problems = append(problems, fmt.Errorf("reason exceeds %d bytes", maxApprovalReasonLen))
	}
	if req.ExpiresIn <= 0 || req.ExpiresIn > maxApprovalTTL {
		problems = append(problems, fmt.Errorf("expires_in must be between 1 and %s", maxApprovalTTL))
	}
	if len(problems) > 0 {
		return codedError(ErrorCodeInvalidArgument, "effect: invalid approval request", errors.Join(problems...))
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("effect: generate approval token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
