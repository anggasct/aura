package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/runtime/checkpoint"
	"github.com/anggasct/aura/internal/store"
)

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

func (c *Checkpoint) ValidateResume(validation *runtimecheckpoint.ResumeValidation) error {
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
	if event.Kind != runtimecheckpoint.EventKindRunCheckpoint {
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
