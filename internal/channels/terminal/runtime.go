package terminal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type DenyPolicy struct{}

func (DenyPolicy) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return approval.PolicyDecision{Outcome: approval.OutcomeDeny, ReasonCode: "plain_console_denies_tools"}, nil
}

type RuntimeRunner struct {
	engine  turn.AgentRuntime
	agentID string
}

func NewRuntimeRunner(engine turn.AgentRuntime, agentID string) (*RuntimeRunner, error) {
	if engine == nil {
		return nil, errNilArgument("engine")
	}
	return &RuntimeRunner{engine: engine, agentID: agentID}, nil
}

func (r *RuntimeRunner) Run(ctx context.Context, req *Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if req == nil {
			yield(Event{}, errNilArgument("request"))
			return
		}
		parts := make([]runtimeingress.InputPart, len(req.Parts))
		for i := range req.Parts {
			parts[i] = runtimeingress.InputPart{Text: req.Parts[i].Text, Image: req.Parts[i].Image}
		}
		agentID := req.AgentID
		if agentID == "" {
			agentID = r.agentID
		}
		runtimeReq := &turn.TurnRequest{
			SessionID:      req.SessionID,
			PrincipalID:    req.PrincipalID,
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          parts,
			IdempotencyKey: req.IdempotencyKey,
			AgentID:        agentID,
			Skill:          req.Skill,
		}
		for ev, err := range r.engine.Run(ctx, runtimeReq) {
			if err != nil {
				yield(Event{}, err)
				return
			}
			if !yield(Event{
				Kind:    ev.Kind,
				Author:  ev.Author,
				TurnID:  ev.TurnID,
				Payload: ev.Payload,
			}, nil) {
				return
			}
		}
	}
}

type TurnSessions struct {
	sessions turnstore.SessionService
	newID    func() (string, error)
	agentID  string
}

var _ Sessions = (*TurnSessions)(nil)

func NewTurnSessions(sessions turnstore.SessionService, agentID string) (*TurnSessions, error) {
	if sessions == nil {
		return nil, errNilArgument("sessions")
	}
	return &TurnSessions{sessions: sessions, agentID: agentID}, nil
}

func (s *TurnSessions) Create(ctx context.Context, owner, agentID string) (Session, error) {
	newID := s.newID
	if newID == nil {
		newID = newSessionID
	}
	id, err := newID()
	if err != nil {
		return Session{}, fmt.Errorf("terminal: create session id: %w", err)
	}
	if agentID == "" {
		agentID = s.agentID
	}
	internal := &turn.Session{
		ID:        id,
		OwnerID:   owner,
		AgentID:   agentID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.sessions.Create(ctx, internal); err != nil {
		return Session{}, err
	}
	return Session{ID: internal.ID, OwnerID: internal.OwnerID, AgentID: internal.AgentID}, nil
}

func (s *TurnSessions) Get(ctx context.Context, id string) (Session, error) {
	internal, err := s.sessions.Get(ctx, id)
	if err != nil {
		if code, ok := fault.CodeOf[turnstore.ErrorCode](err); errors.Is(err, sql.ErrNoRows) || (ok && code == turnstore.ErrorCodeSessionNotFound) {
			return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return Session{}, err
	}
	return Session{ID: internal.ID, OwnerID: internal.OwnerID, AgentID: internal.AgentID}, nil
}

func (s *TurnSessions) ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]Event, error) {
	events, err := s.sessions.ListEvents(ctx, sessionID, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Event, len(events))
	for i := range events {
		out[i] = Event{
			Kind:    events[i].Kind,
			Author:  events[i].Author,
			TurnID:  events[i].TurnID,
			Payload: events[i].Payload,
		}
	}
	return out, nil
}

func ApprovalReachable(approvals *ApprovalBridge) func(sessionID string) bool {
	if approvals == nil {
		return nil
	}
	return func(string) bool { return true }
}

func ApprovalDecider(approvals *ApprovalBridge) toolbroker.ApprovalDecider {
	if approvals == nil {
		return nil
	}
	return func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if prompt == nil {
			return false, errNilArgument("approval prompt")
		}
		return approvals.Decide(ctx, &ApprovalCard{
			ToolName:       prompt.ToolName,
			ToolVersion:    prompt.ToolVersion,
			SessionID:      prompt.SessionID,
			TurnID:         prompt.TurnID,
			PrincipalID:    prompt.PrincipalID,
			Arguments:      prompt.Arguments,
			Network:        prompt.Network,
			Timeout:        prompt.Timeout,
			MaxOutputBytes: prompt.MaxOutputBytes,
			PolicyVersion:  prompt.PolicyVersion,
			ReasonCode:     prompt.ReasonCode,
			ExpiresAt:      prompt.ExpiresAt,
		})
	}
}

func newSessionID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sess_" + hex.EncodeToString(b[:]), nil
}
