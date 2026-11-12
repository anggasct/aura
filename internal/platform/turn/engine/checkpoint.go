package runtimeengine

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/checkpoint"
	"github.com/anggasct/aura/internal/store"
)

const checkpointSchemaVersion uint16 = 1

func (e *Engine) SaveCheckpoint(ctx context.Context, checkpoint *turn.Checkpoint) error {
	if ctx == nil {
		return invalidArgument("context must not be nil")
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	payload, err := checkpoint.MarshalPayload()
	if err != nil {
		return err
	}

	e.mu.Lock()
	if e.shutdown {
		e.mu.Unlock()
		return codedError(turn.ErrorCodeRuntimeOverloaded, "runtime is shutting down", nil)
	}
	sq := e.sessions[checkpoint.SessionID]
	if sq == nil {
		sq = &sessionQueue{}
		e.sessions[checkpoint.SessionID] = sq
	}
	e.mu.Unlock()

	return sq.lock(func() error {
		last, err := e.events.LastSequence(ctx, checkpoint.SessionID)
		if err != nil {
			return codedError(turn.ErrorCodeStorageUnavailable, "read checkpoint event boundary", err)
		}
		if last < checkpoint.EventSequence {
			return codedError(turn.ErrorCodeCheckpointStale, "checkpoint boundary is ahead of the event log", nil)
		}
		eventID := checkpoint.RunID + ".checkpoint." + strconv.FormatUint(checkpoint.ResumeGeneration, 10)
		appender, ok := e.events.(runtimecheckpoint.CheckpointAppender)
		if !ok {
			return codedError(turn.ErrorCodeStorageUnavailable, "event store cannot atomically append checkpoints", nil)
		}
		sequence, err := e.nextSequence(ctx, checkpoint.SessionID)
		if err != nil {
			return codedError(turn.ErrorCodeStorageUnavailable, "allocate checkpoint event sequence", err)
		}
		event := &store.RuntimeEvent{
			ID:            eventID,
			SessionID:     checkpoint.SessionID,
			Sequence:      sequence,
			TurnID:        checkpoint.TurnID,
			InvocationID:  checkpoint.RunID,
			Branch:        "checkpoint",
			Author:        "runtime",
			Kind:          runtimecheckpoint.EventKindRunCheckpoint,
			SchemaVersion: checkpointSchemaVersion,
			Payload:       payload,
			CreatedAt:     time.Now().UTC(),
		}
		if err := appender.AppendCheckpoint(ctx, event); err != nil {
			if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeEventSequenceConflict {
				return codedError(turn.ErrorCodeCheckpointStale, "checkpoint id is already bound to different state", err)
			}
			return codedError(turn.ErrorCodeStorageUnavailable, "append checkpoint event", err)
		}
		return nil
	})
}

func (e *Engine) LoadCheckpoint(ctx context.Context, sessionID, turnID string) (turn.Checkpoint, error) {
	if ctx == nil {
		return turn.Checkpoint{}, invalidArgument("context must not be nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return turn.Checkpoint{}, invalidArgument("session id must not be empty")
	}
	if strings.TrimSpace(turnID) == "" {
		return turn.Checkpoint{}, invalidArgument("turn id must not be empty")
	}
	events, err := e.dedupe.ListTurnEvents(ctx, sessionID, turnID)
	if err != nil {
		return turn.Checkpoint{}, codedError(turn.ErrorCodeStorageUnavailable, "list checkpoint events", err)
	}
	var latest turn.Checkpoint
	found := false
	for i := range events {
		if events[i].Kind != runtimecheckpoint.EventKindRunCheckpoint {
			continue
		}
		checkpoint, parseErr := turn.ParseCheckpoint(&events[i])
		if parseErr != nil {
			return turn.Checkpoint{}, parseErr
		}
		if !found || events[i].Sequence > latest.EventSequence {
			latest = checkpoint
			found = true
		}
	}
	if !found {
		return turn.Checkpoint{}, codedError(turn.ErrorCodeCheckpointNotFound, "no checkpoint exists for turn", nil)
	}
	return latest, nil
}

func (e *Engine) ValidateCheckpoint(ctx context.Context, validation *runtimecheckpoint.ResumeValidation) (turn.Checkpoint, error) {
	if validation == nil {
		return turn.Checkpoint{}, invalidArgument("resume validation must not be nil")
	}
	checkpoint, err := e.LoadCheckpoint(ctx, validation.SessionID, validation.TurnID)
	if err != nil {
		return turn.Checkpoint{}, err
	}
	if err := checkpoint.ValidateResume(validation); err != nil {
		return turn.Checkpoint{}, err
	}
	return checkpoint, nil
}

func (e *Engine) ValidateCheckpointWithEffects(ctx context.Context, validation *runtimecheckpoint.ResumeValidation, effects runtimecheckpoint.EffectResumeValidator) (turn.Checkpoint, error) {
	if effects == nil {
		return turn.Checkpoint{}, invalidArgument("effect resume validator must not be nil")
	}
	if validation == nil {
		return turn.Checkpoint{}, invalidArgument("resume validation must not be nil")
	}
	checkpoint, err := e.LoadCheckpoint(ctx, validation.SessionID, validation.TurnID)
	if err != nil {
		return turn.Checkpoint{}, err
	}
	validated := *validation
	if err := effects.ValidateResumeEffects(ctx, checkpoint.SessionID, checkpoint.TurnID, checkpoint.PendingToolCallIDs); err != nil {
		return turn.Checkpoint{}, codedError(turn.ErrorCodeCheckpointStale, "effect state validation failed", err)
	}
	validated.EffectStateValid = true
	if err := checkpoint.ValidateResume(&validated); err != nil {
		return turn.Checkpoint{}, err
	}
	return checkpoint, nil
}
