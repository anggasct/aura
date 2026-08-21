package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/store"
)

const EventKindRunCheckpoint = "run.checkpoint.v1"

const checkpointSchemaVersion uint16 = 1

type Checkpoint struct {
	RunID              string   `json:"run_id"`
	SessionID          string   `json:"session_id"`
	TurnID             string   `json:"turn_id"`
	OwnerID            string   `json:"owner_id"`
	PrincipalID        string   `json:"principal_id"`
	Phase              string   `json:"phase"`
	EventSequence      uint64   `json:"event_sequence"`
	InputCursor        string   `json:"input_cursor"`
	PendingApprovalIDs []string `json:"pending_approval_ids"`
	PendingToolCallIDs []string `json:"pending_tool_call_ids"`
	CapabilityDigest   string   `json:"capability_digest"`
	PolicyVersion      string   `json:"policy_version"`
	ResumeGeneration   uint64   `json:"resume_generation"`
	StateDigest        string   `json:"state_digest"`
}

type ResumeValidation struct {
	SessionID            string
	TurnID               string
	OwnerID              string
	PrincipalID          string
	CapabilityDigest     string
	PolicyVersion        string
	CurrentEventSequence uint64
	CurrentStateDigest   string
	ResumeGeneration     uint64
	ApprovalValid        bool
	EffectStateValid     bool
}

type EffectResumeValidator interface {
	ValidateResumeEffects(context.Context, string, string, []string) error
}

func (c *Checkpoint) Validate() error {
	if c == nil {
		return invalidArgument("checkpoint must not be nil")
	}
	for name, value := range map[string]string{
		"run_id": c.RunID, "session_id": c.SessionID, "turn_id": c.TurnID,
		"owner_id": c.OwnerID, "principal_id": c.PrincipalID,
		"phase": c.Phase, "input_cursor": c.InputCursor, "policy_version": c.PolicyVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return codedError(ErrorCodeCheckpointInvalid, name+" must not be empty", nil)
		}
		if len(value) > 256 {
			return codedError(ErrorCodeCheckpointInvalid, name+" exceeds its size bound", nil)
		}
	}
	if !validCheckpointPhase(c.Phase) {
		return codedError(ErrorCodeCheckpointInvalid, "checkpoint phase is unsupported", nil)
	}
	if c.EventSequence == 0 || c.ResumeGeneration == 0 {
		return codedError(ErrorCodeCheckpointInvalid, "checkpoint sequence and generation must be positive", nil)
	}
	if err := validateDigest(c.CapabilityDigest, "capability digest"); err != nil {
		return err
	}
	if err := validateDigest(c.StateDigest, "state digest"); err != nil {
		return err
	}
	if err := validateCheckpointIDs(c.PendingApprovalIDs, "pending approval ids"); err != nil {
		return err
	}
	return validateCheckpointIDs(c.PendingToolCallIDs, "pending tool call ids")
}

func (c *Checkpoint) ValidateResume(validation *ResumeValidation) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if validation == nil {
		return invalidArgument("resume validation must not be nil")
	}
	if validation.SessionID != c.SessionID || validation.TurnID != c.TurnID {
		return codedError(ErrorCodeCheckpointStale, "resume identity does not match checkpoint", nil)
	}
	if validation.OwnerID == "" || validation.PrincipalID == "" || validation.OwnerID != c.OwnerID || validation.PrincipalID != c.PrincipalID {
		return codedError(ErrorCodePolicyDenied, "resume principal does not match checkpoint identity", nil)
	}
	if validation.CapabilityDigest != c.CapabilityDigest || validation.PolicyVersion != c.PolicyVersion {
		return codedError(ErrorCodeCheckpointStale, "capability or policy binding changed", nil)
	}
	if validation.CurrentEventSequence < c.EventSequence || validation.CurrentStateDigest != c.StateDigest {
		return codedError(ErrorCodeCheckpointStale, "checkpoint state is no longer current", nil)
	}
	if validation.ResumeGeneration != c.ResumeGeneration {
		return codedError(ErrorCodeCheckpointStale, "checkpoint resume generation was already consumed", nil)
	}
	if !validation.ApprovalValid {
		return codedError(ErrorCodeCheckpointStale, "checkpoint approval is no longer valid", nil)
	}
	if !validation.EffectStateValid {
		return codedError(ErrorCodeCheckpointStale, "active effect state is not recovered", nil)
	}
	return nil
}

func (c *Checkpoint) NextResumeGeneration() (uint64, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if c.ResumeGeneration == ^uint64(0) {
		return 0, codedError(ErrorCodeCheckpointInvalid, "resume generation overflow", nil)
	}
	return c.ResumeGeneration + 1, nil
}

func (c *Checkpoint) MarshalPayload() (json.RawMessage, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal checkpoint: %w", err)
	}
	return payload, nil
}

func ParseCheckpoint(event *store.RuntimeEvent) (Checkpoint, error) {
	if event == nil {
		return Checkpoint{}, invalidArgument("checkpoint event must not be nil")
	}
	if event.Kind != EventKindRunCheckpoint {
		return Checkpoint{}, codedError(ErrorCodeCheckpointUnsupported, "event is not a checkpoint", nil)
	}
	if event.SchemaVersion != checkpointSchemaVersion {
		return Checkpoint{}, codedError(ErrorCodeCheckpointUnsupported, "checkpoint schema version is unsupported", nil)
	}
	if !json.Valid(event.Payload) {
		return Checkpoint{}, codedError(ErrorCodeCheckpointInvalid, "checkpoint payload is not valid JSON", nil)
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(event.Payload, &checkpoint); err != nil {
		return Checkpoint{}, codedError(ErrorCodeCheckpointInvalid, "decode checkpoint payload", err)
	}
	if err := checkpoint.Validate(); err != nil {
		return Checkpoint{}, err
	}
	if checkpoint.SessionID != event.SessionID || checkpoint.TurnID != event.TurnID || checkpoint.RunID != event.InvocationID {
		return Checkpoint{}, codedError(ErrorCodeCheckpointInvalid, "checkpoint event identity does not match payload", nil)
	}
	if event.Sequence <= checkpoint.EventSequence {
		return Checkpoint{}, codedError(ErrorCodeCheckpointInvalid, "checkpoint event sequence must follow its boundary", nil)
	}
	return checkpoint, nil
}

func (e *Engine) SaveCheckpoint(ctx context.Context, checkpoint *Checkpoint) error {
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
		return codedError(ErrorCodeRuntimeOverloaded, "runtime is shutting down", nil)
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
			return codedError(ErrorCodeStorageUnavailable, "read checkpoint event boundary", err)
		}
		if last < checkpoint.EventSequence {
			return codedError(ErrorCodeCheckpointStale, "checkpoint boundary is ahead of the event log", nil)
		}
		eventID := checkpoint.RunID + ".checkpoint." + strconv.FormatUint(checkpoint.ResumeGeneration, 10)
		appender, ok := e.events.(CheckpointAppender)
		if !ok {
			return codedError(ErrorCodeStorageUnavailable, "event store cannot atomically append checkpoints", nil)
		}
		sequence, err := e.nextSequence(ctx, checkpoint.SessionID)
		if err != nil {
			return codedError(ErrorCodeStorageUnavailable, "allocate checkpoint event sequence", err)
		}
		event := &store.RuntimeEvent{
			ID:            eventID,
			SessionID:     checkpoint.SessionID,
			Sequence:      sequence,
			TurnID:        checkpoint.TurnID,
			InvocationID:  checkpoint.RunID,
			Branch:        "checkpoint",
			Author:        "runtime",
			Kind:          EventKindRunCheckpoint,
			SchemaVersion: checkpointSchemaVersion,
			Payload:       payload,
			CreatedAt:     time.Now().UTC(),
		}
		if err := appender.AppendCheckpoint(ctx, event); err != nil {
			if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeEventSequenceConflict {
				return codedError(ErrorCodeCheckpointStale, "checkpoint id is already bound to different state", err)
			}
			return codedError(ErrorCodeStorageUnavailable, "append checkpoint event", err)
		}
		return nil
	})
}

type CheckpointAppender interface {
	AppendCheckpoint(context.Context, *store.RuntimeEvent) error
}

func (e *Engine) LoadCheckpoint(ctx context.Context, turnID string) (Checkpoint, error) {
	if ctx == nil {
		return Checkpoint{}, invalidArgument("context must not be nil")
	}
	if strings.TrimSpace(turnID) == "" {
		return Checkpoint{}, invalidArgument("turn id must not be empty")
	}
	events, err := e.dedupe.ListTurnEvents(ctx, turnID)
	if err != nil {
		return Checkpoint{}, codedError(ErrorCodeStorageUnavailable, "list checkpoint events", err)
	}
	var latest Checkpoint
	found := false
	for i := range events {
		if events[i].Kind != EventKindRunCheckpoint {
			continue
		}
		checkpoint, parseErr := ParseCheckpoint(&events[i])
		if parseErr != nil {
			return Checkpoint{}, parseErr
		}
		if !found || events[i].Sequence > latest.EventSequence {
			latest = checkpoint
			found = true
		}
	}
	if !found {
		return Checkpoint{}, codedError(ErrorCodeCheckpointNotFound, "no checkpoint exists for turn", nil)
	}
	return latest, nil
}

func (e *Engine) ValidateCheckpoint(ctx context.Context, turnID string, validation *ResumeValidation) (Checkpoint, error) {
	checkpoint, err := e.LoadCheckpoint(ctx, turnID)
	if err != nil {
		return Checkpoint{}, err
	}
	if err := checkpoint.ValidateResume(validation); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

func (e *Engine) ValidateCheckpointWithEffects(ctx context.Context, turnID string, validation *ResumeValidation, effects EffectResumeValidator) (Checkpoint, error) {
	if effects == nil {
		return Checkpoint{}, invalidArgument("effect resume validator must not be nil")
	}
	checkpoint, err := e.LoadCheckpoint(ctx, turnID)
	if err != nil {
		return Checkpoint{}, err
	}
	if validation == nil {
		return Checkpoint{}, invalidArgument("resume validation must not be nil")
	}
	validated := *validation
	if err := effects.ValidateResumeEffects(ctx, checkpoint.SessionID, checkpoint.TurnID, checkpoint.PendingToolCallIDs); err != nil {
		return Checkpoint{}, codedError(ErrorCodeCheckpointStale, "effect state validation failed", err)
	}
	validated.EffectStateValid = true
	if err := checkpoint.ValidateResume(&validated); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

func validCheckpointPhase(phase string) bool {
	return slices.Contains([]string{
		"admitting", "awaiting_approval", "preparing", "executing",
		"normalizing", "observing", "settling", "paused",
	}, phase)
}

func validateCheckpointIDs(ids []string, label string) error {
	if len(ids) > 128 {
		return codedError(ErrorCodeCheckpointInvalid, label+" exceed the count bound", nil)
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || len(id) > 256 {
			return codedError(ErrorCodeCheckpointInvalid, label+" contain an invalid id", nil)
		}
		if _, exists := seen[id]; exists {
			return codedError(ErrorCodeCheckpointInvalid, label+" contain a duplicate id", nil)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validateDigest(digest, label string) error {
	if len(digest) != sha256.Size*2 {
		return codedError(ErrorCodeCheckpointInvalid, label+" must be a SHA-256 hex digest", nil)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return codedError(ErrorCodeCheckpointInvalid, label+" must be a SHA-256 hex digest", err)
	}
	return nil
}
