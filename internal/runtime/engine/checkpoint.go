package runtimeengine

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/checkpoint"
	"github.com/anggasct/aura/internal/store"
)

const checkpointSchemaVersion uint16 = 1

func (e *Engine) SaveCheckpoint(ctx context.Context, checkpoint *runtime.Checkpoint) error {
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
		return codedError(runtime.ErrorCodeRuntimeOverloaded, "runtime is shutting down", nil)
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
			return codedError(runtime.ErrorCodeStorageUnavailable, "read checkpoint event boundary", err)
		}
		if last < checkpoint.EventSequence {
			return codedError(runtime.ErrorCodeCheckpointStale, "checkpoint boundary is ahead of the event log", nil)
		}
		eventID := checkpoint.RunID + ".checkpoint." + strconv.FormatUint(checkpoint.ResumeGeneration, 10)
		appender, ok := e.events.(runtimecheckpoint.CheckpointAppender)
		if !ok {
			return codedError(runtime.ErrorCodeStorageUnavailable, "event store cannot atomically append checkpoints", nil)
		}
		sequence, err := e.nextSequence(ctx, checkpoint.SessionID)
		if err != nil {
			return codedError(runtime.ErrorCodeStorageUnavailable, "allocate checkpoint event sequence", err)
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
				return codedError(runtime.ErrorCodeCheckpointStale, "checkpoint id is already bound to different state", err)
			}
			return codedError(runtime.ErrorCodeStorageUnavailable, "append checkpoint event", err)
		}
		return nil
	})
}

func (e *Engine) LoadCheckpoint(ctx context.Context, turnID string) (runtime.Checkpoint, error) {
	if ctx == nil {
		return runtime.Checkpoint{}, invalidArgument("context must not be nil")
	}
	if strings.TrimSpace(turnID) == "" {
		return runtime.Checkpoint{}, invalidArgument("turn id must not be empty")
	}
	events, err := e.dedupe.ListTurnEvents(ctx, turnID)
	if err != nil {
		return runtime.Checkpoint{}, codedError(runtime.ErrorCodeStorageUnavailable, "list checkpoint events", err)
	}
	var latest runtime.Checkpoint
	found := false
	for i := range events {
		if events[i].Kind != runtimecheckpoint.EventKindRunCheckpoint {
			continue
		}
		checkpoint, parseErr := runtime.ParseCheckpoint(&events[i])
		if parseErr != nil {
			return runtime.Checkpoint{}, parseErr
		}
		if !found || events[i].Sequence > latest.EventSequence {
			latest = checkpoint
			found = true
		}
	}
	if !found {
		return runtime.Checkpoint{}, codedError(runtime.ErrorCodeCheckpointNotFound, "no checkpoint exists for turn", nil)
	}
	return latest, nil
}

func (e *Engine) ValidateCheckpoint(ctx context.Context, turnID string, validation *runtimecheckpoint.ResumeValidation) (runtime.Checkpoint, error) {
	checkpoint, err := e.LoadCheckpoint(ctx, turnID)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	if err := checkpoint.ValidateResume(validation); err != nil {
		return runtime.Checkpoint{}, err
	}
	return checkpoint, nil
}

func (e *Engine) ValidateCheckpointWithEffects(ctx context.Context, turnID string, validation *runtimecheckpoint.ResumeValidation, effects runtimecheckpoint.EffectResumeValidator) (runtime.Checkpoint, error) {
	if effects == nil {
		return runtime.Checkpoint{}, invalidArgument("effect resume validator must not be nil")
	}
	checkpoint, err := e.LoadCheckpoint(ctx, turnID)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	if validation == nil {
		return runtime.Checkpoint{}, invalidArgument("resume validation must not be nil")
	}
	validated := *validation
	if err := effects.ValidateResumeEffects(ctx, checkpoint.SessionID, checkpoint.TurnID, checkpoint.PendingToolCallIDs); err != nil {
		return runtime.Checkpoint{}, codedError(runtime.ErrorCodeCheckpointStale, "effect state validation failed", err)
	}
	validated.EffectStateValid = true
	if err := checkpoint.ValidateResume(&validated); err != nil {
		return runtime.Checkpoint{}, err
	}
	return checkpoint, nil
}
