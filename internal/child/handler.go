package child

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type StartPayload struct {
	ChildID       string `json:"child_id"`
	SessionID     string `json:"session_id"`
	DurableKey    string `json:"durable_key"`
	ReservationID string `json:"reservation_id"`
}

type Executor interface {
	RunSession(ctx stdcontext.Context, sessionID string, deadline time.Time) (Result, error)
	CancelSession(ctx stdcontext.Context, sessionID string) error
}

type Result struct {
	Status      string
	Output      string
	Artifacts   []string
	TokensUsed  int64
	CostMicros  int64
	CompletedAt time.Time
}

type Handler struct {
	runs HandlerRuns
	exec Executor
}

type HandlerRuns interface {
	SetState(ctx stdcontext.Context, id, state string, now time.Time) error
	GetRun(ctx stdcontext.Context, id string) (HandlerRun, bool, error)
}

type HandlerRun struct {
	ID         string
	SessionID  string
	State      string
	Deadline   time.Time
	GrantsJSON string
}

func NewHandler(runs HandlerRuns, exec Executor) (*Handler, error) {
	if runs == nil {
		return nil, errNilArgument("runs")
	}
	if exec == nil {
		return nil, errNilArgument("executor")
	}
	return &Handler{runs: runs, exec: exec}, nil
}

func parseStartPayload(raw []byte) (StartPayload, error) {
	var payload StartPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return StartPayload{}, fmt.Errorf("child: decode start payload: %w", err)
	}
	if strings.TrimSpace(payload.ChildID) == "" || strings.TrimSpace(payload.SessionID) == "" {
		return StartPayload{}, Errorf(ErrorCodeChildInvalid, "child start payload is incomplete")
	}
	return payload, nil
}

func terminalFor(result *Result, err error) string {
	if err != nil {
		return StatusFailed
	}
	switch result.Status {
	case "completed":
		return StatusSucceeded
	case "cancelled":
		return StatusCancelled
	case "deadline":
		return StatusDeadline
	default:
		return StatusFailed
	}
}

func (h *Handler) HandleInvocation(ctx stdcontext.Context, payload []byte, now func() time.Time) error {
	if ctx == nil {
		return Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if now == nil {
		return Errorf(ErrorCodeInvalidArgument, "clock must not be nil")
	}
	start, err := parseStartPayload(payload)
	if err != nil {
		return err
	}
	run, found, err := h.runs.GetRun(ctx, start.ChildID)
	if err != nil {
		return err
	}
	if !found {
		return Errorf(ErrorCodeChildNotFound, "child is not found")
	}
	if err := h.runs.SetState(ctx, start.ChildID, StatusRunning, now()); err != nil {
		return err
	}
	result, err := h.exec.RunSession(ctx, run.SessionID, run.Deadline)
	state := terminalFor(&result, err)
	if setErr := h.runs.SetState(ctx, start.ChildID, state, now()); setErr != nil {
		return setErr
	}
	return err
}
