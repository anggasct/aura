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
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
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

	var intent *Intent
	var approval *Approval
	err := sqlite.RunInTx(ctx, j.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, j.db)
		var err error
		intent, err = scanIntent(q.QueryRowContext(ctx, selectIntentByIDSQL, request.IntentID))
		if errors.Is(err, sql.ErrNoRows) {
			return codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", request.IntentID), nil)
		}
		if err != nil {
			return fmt.Errorf("effect: read intent %s: %w", request.IntentID, err)
		}
		if intent.State != StateUnknown {
			return codedError(ErrorCodeTransitionInvalid,
				fmt.Sprintf("effect: intent %s is %s, approval requires unknown", request.IntentID, intent.State), nil)
		}

		ownerID, err := j.turns.SessionOwner(ctx, intent.SessionID)
		if err != nil {
			return fmt.Errorf("effect: read owner for intent %s: %w", req.IntentID, err)
		}
		token, err := randomToken()
		if err != nil {
			return err
		}
		now := j.clock.Now().UTC()
		expiresAt := now.Add(request.ExpiresIn)
		approvalID, err := randomID()
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `
			INSERT INTO effect_approval (
			    id, intent_id, action, request_digest, owner_id, reason, token_hash,
			    issued_at, expires_at, consumed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			approvalID, intent.ID, string(request.Action), intent.RequestDigest, ownerID, request.Reason,
			hashToken(token), fmtTime(now), fmtTime(expiresAt),
		)
		if err != nil {
			return fmt.Errorf("effect: insert approval: %w", err)
		}
		approval = &Approval{
			ID:            approvalID,
			IntentID:      intent.ID,
			Action:        request.Action,
			RequestDigest: intent.RequestDigest,
			OwnerID:       ownerID,
			Reason:        request.Reason,
			IssuedAt:      now,
			ExpiresAt:     expiresAt,
			Token:         token,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	j.observe(ctx, intent, "approval_issued")
	return approval, nil
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

	var resolved *Intent
	err := sqlite.RunInTx(ctx, j.db, func(ctx context.Context) error {
		q := sqlite.Conn(ctx, j.db)
		approval, ownerID, err := readApproval(ctx, q, j.turns, hashToken(token))
		if errors.Is(err, sql.ErrNoRows) {
			return codedError(ErrorCodeApprovalInvalid, "effect: approval token is invalid", nil)
		}
		if err != nil {
			return fmt.Errorf("effect: read approval: %w", err)
		}
		if approval.ConsumedAt != nil {
			return codedError(ErrorCodeApprovalConsumed, "effect: approval token was already consumed", nil)
		}
		intent, err := scanIntent(q.QueryRowContext(ctx, selectIntentByIDSQL, id))
		if errors.Is(err, sql.ErrNoRows) {
			return codedError(ErrorCodeNotFound, fmt.Sprintf("effect: intent %s not found", id), nil)
		}
		if err != nil {
			return fmt.Errorf("effect: read intent %s: %w", id, err)
		}
		if approval.IntentID != intent.ID || approval.Action != action || approval.RequestDigest != intent.RequestDigest || approval.OwnerID != ownerID {
			return codedError(ErrorCodeApprovalInvalid, "effect: approval token binding does not match intent", nil)
		}
		now := j.clock.Now().UTC()
		if !now.Before(approval.ExpiresAt) {
			return codedError(ErrorCodeApprovalExpired, "effect: approval token has expired", nil)
		}
		if action != ApprovalActionRetry && approval.Reason != reason {
			return codedError(ErrorCodeApprovalInvalid, "effect: approval reason does not match", nil)
		}
		if intent.State != StateUnknown {
			return codedError(ErrorCodeTransitionInvalid,
				fmt.Sprintf("effect: intent %s is %s, approval requires unknown", id, intent.State), nil)
		}

		if action == ApprovalActionRetry {
			resolved, err = createApprovedRetry(ctx, q, j.turns, intent, ownerID, now)
		} else {
			state := StateFailed
			safeErrorCode := "effect_owner_resolved_failed"
			if action == ApprovalActionMarkSucceeded {
				state = StateSucceeded
				safeErrorCode = ""
			}
			result, updateErr := q.ExecContext(ctx, `
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
			return err
		}

		result, err := q.ExecContext(ctx,
			`UPDATE effect_approval SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`,
			fmtTime(now), approval.ID,
		)
		if err != nil {
			return fmt.Errorf("effect: consume approval: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("effect: read approval consumption: %w", err)
		}
		if affected != 1 {
			return codedError(ErrorCodeApprovalConsumed, "effect: approval token was consumed concurrently", nil)
		}
		return nil
	})
	if err != nil {
		return nil, err
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

func readApproval(ctx context.Context, q queryer, turns TurnStore, tokenHash string) (*approvalRecord, string, error) {
	var record approvalRecord
	var action, expiresAt, sessionID string
	var consumedAt sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT a.id, a.intent_id, a.action, a.request_digest, a.owner_id, a.reason,
		       a.expires_at, a.consumed_at, i.session_id
		FROM effect_approval a
		JOIN effect_intent i ON i.id = a.intent_id
		WHERE a.token_hash = ?`, tokenHash).Scan(
		&record.ID, &record.IntentID, &action, &record.RequestDigest, &record.OwnerID, &record.Reason,
		&expiresAt, &consumedAt, &sessionID,
	)
	if err != nil {
		return nil, "", err
	}
	ownerID, err := turns.SessionOwner(ctx, sessionID)
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

func createApprovedRetry(ctx context.Context, q sqlite.Querier, turns TurnStore, intent *Intent, ownerID string, now time.Time) (*Intent, error) {
	normalizedRequest, err := normalizeRequest(intent.RequestJSON)
	if err != nil {
		return nil, err
	}
	requestDigest := digestRequest(normalizedRequest)
	if requestDigest != intent.RequestDigest {
		return nil, codedError(ErrorCodeApprovalInvalid, "effect: stored request does not match approval digest", nil)
	}
	newID, err := randomID()
	if err != nil {
		return nil, err
	}
	newKey, err := randomID()
	if err != nil {
		return nil, err
	}
	sequence, err := turns.AllocateSequence(ctx, intent.SessionID)
	if err != nil {
		return nil, fmt.Errorf("effect: allocate retry event sequence: %w", err)
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
	if err := turns.AppendEvent(ctx, &turn.Event{
		ID:            eventID,
		SessionID:     intent.SessionID,
		Sequence:      sequence,
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
	_, err = q.ExecContext(ctx, insertIntentSQLWithRetry,
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
