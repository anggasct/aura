package child

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/durable"
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
	Status      string    `json:"status"`
	Output      string    `json:"output"`
	Artifacts   []string  `json:"artifacts"`
	TokensUsed  int64     `json:"tokens_used"`
	CostMicros  int64     `json:"cost_micros"`
	CompletedAt time.Time `json:"completed_at"`
}

type Handler struct {
	runs   HandlerRuns
	exec   Executor
	ledger BudgetLedger
}

type HandlerRuns interface {
	SetState(ctx stdcontext.Context, id, state string, now time.Time) error
	GetRun(ctx stdcontext.Context, id string) (HandlerRun, bool, error)
}

type HandlerResultStore interface {
	SetResult(ctx stdcontext.Context, id string, result *Result, now time.Time) error
}

type HandlerRun struct {
	ID            string
	SessionID     string
	State         string
	Deadline      time.Time
	GrantsJSON    string
	ContextDigest string
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

func NewHandlerWithLedger(runs HandlerRuns, exec Executor, ledger BudgetLedger) (*Handler, error) {
	h, err := NewHandler(runs, exec)
	if err != nil {
		return nil, err
	}
	h.ledger = ledger
	return h, nil
}

var ErrNonResumable = errors.New("child: non-resumable attempt")

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
		if errors.Is(err, stdcontext.Canceled) || errors.Is(err, stdcontext.DeadlineExceeded) {
			return StatusCancelled
		}
		if errors.Is(err, ErrNonResumable) {
			return StatusInterrupted
		}
		return StatusFailed
	}
	switch result.Status {
	case "completed":
		return StatusSucceeded
	case "cancelled":
		return StatusCancelled
	case "deadline":
		return StatusDeadline
	case "interrupted":
		return StatusInterrupted
	default:
		return StatusFailed
	}
}

func isTerminalState(state string) bool {
	switch state {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusDeadline, StatusInterrupted:
		return true
	default:
		return false
	}
}

func checkResult(result *Result) error {
	if result.TokensUsed < 0 || result.CostMicros < 0 {
		return Errorf(ErrorCodeChildInvalid, "child result usage must not be negative")
	}
	if len(result.Output) > 8192 {
		return Errorf(ErrorCodeChildInvalid, "child result output exceeds the bound")
	}
	if len(result.Artifacts) > 32 {
		return Errorf(ErrorCodeChildInvalid, "child result artifacts exceed the bound")
	}
	return nil
}

func (h *Handler) HandleInvocation(ctx stdcontext.Context, payload []byte, now func() time.Time) error {
	return h.handle(ctx, nil, payload, now)
}

func (h *Handler) HandleDurable(ctx stdcontext.Context, inv durable.Invocation, now func() time.Time) error {
	if inv == nil {
		return Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	return h.handle(ctx, inv, inv.Payload(), now)
}

func (h *Handler) handle(ctx stdcontext.Context, inv durable.Invocation, payload []byte, now func() time.Time) error {
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
	if isTerminalState(run.State) {
		return nil
	}
	current := now().UTC()
	if !run.Deadline.IsZero() && !current.Before(run.Deadline.UTC()) {
		if setErr := h.runs.SetState(ctx, start.ChildID, StatusDeadline, current); setErr != nil {
			return setErr
		}
		h.releaseLedger(ctx, start.ReservationID)
		return Errorf(ErrorCodeChildInvalid, "child deadline has passed")
	}
	if err := h.runs.SetState(ctx, start.ChildID, StatusRunning, current); err != nil {
		return err
	}
	result, execErr := h.runJournaled(ctx, inv, run.SessionID, run.Deadline)
	if execErr == nil {
		if checkErr := checkResult(&result); checkErr != nil {
			execErr = checkErr
		}
	}
	state := terminalFor(&result, execErr)
	if execErr == nil && !run.Deadline.IsZero() && !now().UTC().Before(run.Deadline.UTC()) && state == StatusSucceeded {
		state = StatusDeadline
	}
	settled := now().UTC()
	if result.CompletedAt.IsZero() {
		result.CompletedAt = settled
	}
	if execErr == nil {
		h.chargeLedger(ctx, start.ReservationID, result.TokensUsed, result.CostMicros)
	}
	h.releaseLedger(ctx, start.ReservationID)
	if store, ok := h.runs.(HandlerResultStore); ok && execErr == nil {
		if setErr := store.SetResult(ctx, start.ChildID, &result, settled); setErr != nil {
			if stateErr := h.runs.SetState(ctx, start.ChildID, StatusFailed, settled); stateErr != nil {
				return stateErr
			}
			return setErr
		}
	}
	if setErr := h.runs.SetState(ctx, start.ChildID, state, settled); setErr != nil {
		return setErr
	}
	return execErr
}

func (h *Handler) runJournaled(ctx stdcontext.Context, inv durable.Invocation, sessionID string, deadline time.Time) (Result, error) {
	if inv == nil {
		return h.exec.RunSession(ctx, sessionID, deadline)
	}
	key := "child-exec/" + sessionID
	raw, err := inv.RunAction(ctx, key, func(ctx stdcontext.Context) ([]byte, error) {
		res, runErr := h.exec.RunSession(ctx, sessionID, deadline)
		if runErr != nil {
			return nil, runErr
		}
		encoded, encErr := json.Marshal(res)
		if encErr != nil {
			return nil, encErr
		}
		return encoded, nil
	})
	if err != nil {
		return Result{}, err
	}
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (h *Handler) chargeLedger(ctx stdcontext.Context, reservationID string, tokens, cost int64) {
	if h.ledger == nil || reservationID == "" {
		return
	}
	_ = h.ledger.Charge(ctx, reservationID, tokens, cost)
}

func (h *Handler) releaseLedger(ctx stdcontext.Context, reservationID string) {
	if h.ledger == nil || reservationID == "" {
		return
	}
	_ = h.ledger.Release(ctx, reservationID)
}
