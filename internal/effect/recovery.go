package effect

import (
	"context"
	"encoding/json"
	"fmt"
)

// Claim's conditional UPDATE (state = 'started') makes the started->unknown
// transition exactly-once across concurrent callers.
func (j *Journal) Claim(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, codedError(ErrorCodeInvalidArgument, "effect: id must not be empty", nil)
	}
	res, err := j.db.ExecContext(ctx, `
		UPDATE effect_intent
		SET state = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		string(StateUnknown), fmtTime(j.now()), id, string(StateStarted),
	)
	if err != nil {
		return false, fmt.Errorf("effect: claim intent %s: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("effect: read claim rows affected: %w", err)
	}
	return affected == 1, nil
}

type RecoveryReport struct {
	Scanned int
	Claimed int
}

// Recover marks every still-started intent unknown: started is ambiguous after
// a crash (the provider may already have observed the request), and without
// reconciliation the only safe resolution is unknown.
func (j *Journal) Recover(ctx context.Context) (RecoveryReport, error) {
	started, err := j.ListByState(ctx, StateStarted, 0)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{Scanned: len(started)}
	for i := range started {
		claimed, err := j.Claim(ctx, started[i].ID)
		if err != nil {
			return report, err
		}
		if claimed {
			report.Claimed++
		}
	}
	return report, nil
}

func (j *Journal) ValidateResumeEffects(ctx context.Context, sessionID, turnID string, toolCallIDs []string) error {
	if sessionID == "" || turnID == "" {
		return codedError(ErrorCodeInvalidArgument, "effect: session and turn must not be empty", nil)
	}
	if len(toolCallIDs) > 128 {
		return codedError(ErrorCodeInvalidArgument, "effect: too many pending tool calls", nil)
	}
	if len(toolCallIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(toolCallIDs))
	for _, id := range toolCallIDs {
		if id == "" {
			return codedError(ErrorCodeInvalidArgument, "effect: pending tool call id must not be empty", nil)
		}
		if _, exists := seen[id]; exists {
			return codedError(ErrorCodeInvalidArgument, "effect: duplicate pending tool call id", nil)
		}
		seen[id] = struct{}{}
	}

	query := `
	SELECT id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation,
	       classification, state, request_digest, request_json, provider_receipt_json,
	       safe_error_code, retry_of, prepared_at, started_at, finished_at, reconciled_at, updated_at
	FROM effect_intent
	WHERE session_id = ? AND turn_id = ?
	  AND tool_call_id IN (SELECT value FROM json_each(?))`
	idsJSON, err := json.Marshal(toolCallIDs)
	if err != nil {
		return fmt.Errorf("effect: encode pending tool calls: %w", err)
	}
	rows, err := j.db.QueryContext(ctx, query, sessionID, turnID, string(idsJSON))
	if err != nil {
		return fmt.Errorf("effect: list pending resume effects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found := make(map[string]struct{}, len(toolCallIDs))
	var intents []Intent
	for rows.Next() {
		intent, err := scanIntent(rows)
		if err != nil {
			return fmt.Errorf("effect: scan pending resume effect: %w", err)
		}
		found[intent.ToolCallID] = struct{}{}
		intents = append(intents, *intent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("effect: list pending resume effects: %w", err)
	}
	for _, id := range toolCallIDs {
		if _, exists := found[id]; !exists {
			return codedError(ErrorCodeNotFound, fmt.Sprintf("effect: pending tool call %s not found", id), nil)
		}
	}
	for i := range intents {
		switch intents[i].State {
		case StatePrepared, StateUnknown:
			continue
		case StateStarted:
			claimed, err := j.Claim(ctx, intents[i].ID)
			if err != nil {
				return err
			}
			if claimed {
				continue
			}
			current, err := j.Get(ctx, intents[i].ID)
			if err != nil {
				return err
			}
			if current.State == StateUnknown {
				continue
			}
			return codedError(ErrorCodeTransitionInvalid, fmt.Sprintf("effect: pending tool call %s changed to %s during recovery", intents[i].ToolCallID, current.State), nil)
		case StateSucceeded, StateFailed:
			return codedError(ErrorCodeTransitionInvalid, fmt.Sprintf("effect: pending tool call %s is already terminal (%s)", intents[i].ToolCallID, intents[i].State), nil)
		default:
			return codedError(ErrorCodeTransitionInvalid, fmt.Sprintf("effect: pending tool call %s has unsupported state %s", intents[i].ToolCallID, intents[i].State), nil)
		}
	}
	return nil
}
