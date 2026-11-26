package runtimeengine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

type TurnRunStore interface {
	RunsOf(ctx context.Context, handler string) ([]durable.HandlerRun, error)
	DeleteRuns(ctx context.Context, keys []string) (int, error)
}

type AcceptedTurnReader interface {
	AcceptedTurns(ctx context.Context, turns []turn.SessionTurn) ([]turn.SessionTurn, error)
}

func DeleteOrphanedTurnRuns(ctx context.Context, db *sql.DB, runs TurnRunStore, turns AcceptedTurnReader) (int, error) {
	if db == nil {
		return 0, fault.New(turn.ErrorCodeInvalidArgument, "db must not be nil")
	}
	if runs == nil {
		return 0, fault.New(turn.ErrorCodeInvalidArgument, "runs must not be nil")
	}
	if turns == nil {
		return 0, fault.New(turn.ErrorCodeInvalidArgument, "turns must not be nil")
	}
	var deleted int
	err := sqlite.RunInTx(ctx, db, func(ctx context.Context) error {
		listed, err := runs.RunsOf(ctx, HandlerName)
		if err != nil {
			return err
		}
		var candidates []turn.SessionTurn
		for _, run := range listed {
			sessionID, ok := orphanedTurnRunSessionID(run.Payload)
			if !ok {
				continue
			}
			candidates = append(candidates, turn.SessionTurn{SessionID: sessionID, TurnID: run.Key})
		}
		if len(candidates) == 0 {
			return nil
		}
		accepted, err := turns.AcceptedTurns(ctx, candidates)
		if err != nil {
			return err
		}
		acceptedSet := make(map[turn.SessionTurn]struct{}, len(accepted))
		for _, pair := range accepted {
			acceptedSet[pair] = struct{}{}
		}
		var orphans []string
		for _, candidate := range candidates {
			if _, ok := acceptedSet[candidate]; !ok {
				orphans = append(orphans, candidate.TurnID)
			}
		}
		if len(orphans) == 0 {
			return nil
		}
		deleted, err = runs.DeleteRuns(ctx, orphans)
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func orphanedTurnRunSessionID(payload []byte) (string, bool) {
	if !json.Valid(payload) {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return "", false
	}
	raw, ok := fields["session_id"]
	if !ok {
		return "", false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false
	}
	if trimmed[0] == '"' {
		var sessionID string
		if err := json.Unmarshal(trimmed, &sessionID); err != nil {
			return "", false
		}
		return sessionID, true
	}
	return string(trimmed), true
}
