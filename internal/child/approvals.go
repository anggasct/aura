package child

import (
	stdcontext "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const approvalBindingTTL = time.Minute

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
	PrincipalID      string
	SessionID        string
	ToolName         string
	ToolVersion      string
	Capabilities     []string
	ExpiresAt        time.Time
	ChildID          string
	ParentInvocation string
	ActionDigest     string
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

func actionDigest(toolName, toolVersion string, args json.RawMessage, capabilities []string) string {
	var digest strings.Builder
	digest.WriteString(strings.ToLower(strings.TrimSpace(toolName)))
	digest.WriteString("\x00")
	digest.WriteString(strings.TrimSpace(toolVersion))
	digest.WriteString("\x00")
	digest.Write(args)
	for _, capability := range capabilities {
		digest.WriteString("\x00")
		digest.WriteString(strings.ToLower(strings.TrimSpace(capability)))
	}
	sum := sha256.Sum256([]byte(digest.String()))
	return hex.EncodeToString(sum[:])
}

func bindChildGrant(spawn *Spawn, request *ToolRequest, expiresAt time.Time) *GrantBinding {
	capabilities := make([]string, 0, len(spawn.Grants))
	for _, grant := range spawn.Grants {
		capabilities = append(capabilities, grant.Capability)
	}
	return &GrantBinding{
		PrincipalID:      spawn.OwnerID,
		SessionID:        spawn.SessionID,
		ToolName:         request.ToolName,
		ToolVersion:      request.ToolVersion,
		Capabilities:     capabilities,
		ExpiresAt:        expiresAt,
		ChildID:          spawn.ID,
		ParentInvocation: spawn.ParentInvocation,
		ActionDigest:     actionDigest(request.ToolName, request.ToolVersion, request.Arguments, request.Capabilities),
	}
}

func grantCapabilities(spawn *Spawn) map[string]bool {
	allowed := make(map[string]bool, len(spawn.Grants))
	for _, grant := range spawn.Grants {
		name := strings.TrimSpace(grant.Capability)
		if name == "" {
			continue
		}
		allowed[strings.ToLower(name)] = true
	}
	return allowed
}

func CheckChildToolCall(spawn *Spawn, gate ApprovalGate, clock Clock) func(ctx stdcontext.Context, request *ToolRequest) error {
	if clock == nil {
		clock = systemClock{}
	}
	var mu sync.Mutex
	seenRequests := map[string]string{}
	seenKeys := map[string]string{}
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
		if ctx == nil {
			return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		callTime := clock.Now().UTC()
		if spawn.SessionID == "" || spawn.OwnerID == "" {
			return Errorf(ErrorCodeInvalidArgument, "child spawn is not bound to a session and owner")
		}
		if !strings.EqualFold(request.SessionID, spawn.SessionID) {
			return Errorf(ErrorCodeChildInvalid, "tool call session does not match the child session")
		}
		if strings.EqualFold(request.ToolName, "spawn_child") {
			return Errorf(ErrorCodeChildSpawnDenied, "child catalogs cannot carry spawn_child")
		}
		if request.PrincipalID == spawn.ID || request.PrincipalID == spawn.SessionID {
			return Errorf(ErrorCodeChildForbidden, "child cannot approve its own tool call")
		}
		if request.PrincipalID != spawn.OwnerID {
			return Errorf(ErrorCodeChildForbidden, "tool call principal does not match the child owner")
		}
		if strings.TrimSpace(request.ToolName) == "" {
			return Errorf(ErrorCodeInvalidArgument, "tool name must not be empty")
		}
		allowed := grantCapabilities(spawn)
		if !allowed[strings.ToLower(strings.TrimSpace(request.ToolName))] {
			return Errorf(ErrorCodeChildInvalid, "tool is outside the child grants")
		}
		for _, capability := range request.Capabilities {
			if !allowed[strings.ToLower(strings.TrimSpace(capability))] {
				return Errorf(ErrorCodeChildInvalid, "tool capability is outside the child grants")
			}
		}
		if !request.Deadline.IsZero() && !callTime.Before(request.Deadline.UTC()) {
			return Errorf(ErrorCodeChildInvalid, "tool call deadline has passed")
		}
		if !spawn.Deadline.IsZero() && !callTime.Before(spawn.Deadline.UTC()) {
			return Errorf(ErrorCodeChildInvalid, "child deadline has passed")
		}
		digest := actionDigest(request.ToolName, request.ToolVersion, request.Arguments, request.Capabilities)
		mu.Lock()
		if err := checkReplay(seenRequests, request.RequestID, digest); err != nil {
			mu.Unlock()
			return err
		}
		if err := checkReplay(seenKeys, request.IdempotencyKey, digest); err != nil {
			mu.Unlock()
			return err
		}
		if request.RequestID != "" {
			seenRequests[request.RequestID] = digest
		}
		if request.IdempotencyKey != "" {
			seenKeys[request.IdempotencyKey] = digest
		}
		mu.Unlock()
		expiresAt := callTime.Add(approvalBindingTTL)
		if !request.Deadline.IsZero() && request.Deadline.UTC().Before(expiresAt) {
			expiresAt = request.Deadline.UTC()
		}
		if !spawn.Deadline.IsZero() && spawn.Deadline.UTC().Before(expiresAt) {
			expiresAt = spawn.Deadline.UTC()
		}
		binding := bindChildGrant(spawn, request, expiresAt)
		if err := gate.Authorize(ctx, request, binding, callTime); err != nil {
			return fmt.Errorf("child: authorize tool call: %w", err)
		}
		return nil
	}
}

func checkReplay(seen map[string]string, id, digest string) error {
	if id == "" {
		return nil
	}
	previous, ok := seen[id]
	if !ok {
		return nil
	}
	if previous != digest {
		return Errorf(ErrorCodeChildInvalid, "tool request does not match the approved action")
	}
	return Errorf(ErrorCodeChildConflict, "tool request was already used")
}

func encodeStartPayload(spawn *Spawn, reservation BudgetReservation) ([]byte, error) {
	payload, err := json.Marshal(struct {
		ChildID       string `json:"child_id"`
		SessionID     string `json:"session_id"`
		DurableKey    string `json:"durable_key"`
		ReservationID string `json:"reservation_id"`
		Deadline      string `json:"deadline"`
	}{
		ChildID: spawn.ID, SessionID: spawn.SessionID,
		DurableKey: spawn.DurableKey, ReservationID: reservation.ID,
		Deadline: spawn.Deadline.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("child: encode start payload: %w", err)
	}
	return payload, nil
}
