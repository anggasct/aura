package child

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ToolRequest struct {
	RequestID      string
	TurnID         string
	SessionID      string
	PrincipalID    string
	ToolName       string
	ToolVersion    string
	Arguments      json.RawMessage
	Capabilities   []string
	Deadline       time.Time
	IdempotencyKey string
}

type GrantBinding struct {
	PrincipalID  string
	SessionID    string
	ToolName     string
	ToolVersion  string
	Capabilities []string
	ExpiresAt    time.Time
}

type ApprovalGate interface {
	Authorize(ctx stdcontext.Context, request *ToolRequest, binding *GrantBinding, now time.Time) error
}

type ApprovalError struct {
	Reason string
}

func (e *ApprovalError) Error() string {
	return "child approval denied: " + e.Reason
}

func bindChildGrant(spawn *Spawn, request *ToolRequest, expiresAt time.Time) *GrantBinding {
	capabilities := make([]string, 0, len(spawn.Grants))
	for _, grant := range spawn.Grants {
		capabilities = append(capabilities, grant.Capability)
	}
	return &GrantBinding{
		PrincipalID:  request.PrincipalID,
		SessionID:    spawn.SessionID,
		ToolName:     request.ToolName,
		ToolVersion:  request.ToolVersion,
		Capabilities: capabilities,
		ExpiresAt:    expiresAt,
	}
}

func CheckChildToolCall(spawn *Spawn, gate ApprovalGate, now time.Time) func(ctx stdcontext.Context, request *ToolRequest) error {
	return func(ctx stdcontext.Context, request *ToolRequest) error {
		if spawn == nil {
			return Errorf(ErrorCodeInvalidArgument, "child spawn must not be nil")
		}
		if gate == nil {
			return Errorf(ErrorCodeInvalidArgument, "approval gate must not be nil")
		}
		if request == nil {
			return Errorf(ErrorCodeInvalidArgument, "tool request must not be nil")
		}
		if now.IsZero() {
			return Errorf(ErrorCodeInvalidArgument, "timestamp must not be zero")
		}
		if !strings.EqualFold(request.SessionID, spawn.SessionID) {
			return Errorf(ErrorCodeChildInvalid, "tool call session does not match the child session")
		}
		if strings.EqualFold(request.ToolName, "spawn_child") {
			return Errorf(ErrorCodeChildSpawnDenied, "child catalogs cannot carry spawn_child")
		}
		binding := bindChildGrant(spawn, request, now.Add(time.Minute))
		binding.SessionID = spawn.SessionID
		if err := gate.Authorize(ctx, request, binding, now); err != nil {
			return fmt.Errorf("child: authorize tool call: %w", err)
		}
		return nil
	}
}

func encodeStartPayload(spawn *Spawn, reservation BudgetReservation) ([]byte, error) {
	payload, err := json.Marshal(struct {
		ChildID       string `json:"child_id"`
		SessionID     string `json:"session_id"`
		DurableKey    string `json:"durable_key"`
		ReservationID string `json:"reservation_id"`
	}{
		ChildID: spawn.ID, SessionID: spawn.SessionID,
		DurableKey: spawn.DurableKey, ReservationID: reservation.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("child: encode start payload: %w", err)
	}
	return payload, nil
}
