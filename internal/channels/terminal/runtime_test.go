package terminal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type fakeTurnSessionService struct {
	created *turn.Session
	getErr  error
}

func (f *fakeTurnSessionService) Create(_ context.Context, sess *turn.Session) error {
	f.created = sess
	return nil
}

func (f *fakeTurnSessionService) Get(context.Context, string) (turn.Session, error) {
	if f.getErr != nil {
		return turn.Session{}, f.getErr
	}
	return turn.Session{}, errors.New("not implemented")
}

func (f *fakeTurnSessionService) ListEvents(context.Context, string, uint64, int) ([]turn.Event, error) {
	return nil, nil
}

func TestTerminalSessionIDFailureIsReturned(t *testing.T) {
	svc := &fakeTurnSessionService{}
	want := errors.New("random source unavailable")
	sessions := &TurnSessions{sessions: svc, newID: func() (string, error) { return "", want }}
	if _, err := sessions.Create(t.Context(), "owner", "main"); !errors.Is(err, want) {
		t.Fatalf("Create error = %v, want %v", err, want)
	}
	if svc.created != nil {
		t.Fatal("session was created after ID generation failed")
	}
}

func TestTerminalSessionGetReportsMissingSessions(t *testing.T) {
	for _, missing := range []error{
		fmt.Errorf("get session x: %w", sql.ErrNoRows),
		fault.New(turnstore.ErrorCodeSessionNotFound, "session does not exist"),
	} {
		sessions := &TurnSessions{sessions: &fakeTurnSessionService{getErr: missing}}
		if _, err := sessions.Get(t.Context(), "x"); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("Get(%v) error = %v, want session not found", missing, err)
		}
	}
	other := errors.New("database is locked")
	sessions := &TurnSessions{sessions: &fakeTurnSessionService{getErr: other}}
	if _, err := sessions.Get(t.Context(), "x"); !errors.Is(err, other) || errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get error = %v, want the store error", err)
	}
}

func TestTerminalApprovalReachableOnlyWhenTheBridgeExists(t *testing.T) {
	if ApprovalReachable(nil) != nil {
		t.Error("a chat without an approval bridge must be unreachable")
	}
	reachable := ApprovalReachable(NewApprovalBridge())
	if reachable == nil || !reachable("any-session") {
		t.Error("a TTY chat with the approval bridge must be reachable for every session")
	}
}

func TestTerminalApprovalDeciderNilWithoutBridge(t *testing.T) {
	if ApprovalDecider(nil) != nil {
		t.Fatal("ApprovalDecider(nil) must return nil")
	}
}

func TestTerminalRuntimeConstructorsRefuseNil(t *testing.T) {
	if _, err := NewRuntimeRunner(nil, "main"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("NewRuntimeRunner(nil) = %v, want invalid_argument", err)
	}
	if _, err := NewTurnSessions(nil, "main"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("NewTurnSessions(nil) = %v, want invalid_argument", err)
	}
	decision, err := (DenyPolicy{}).Evaluate(t.Context(), &approval.ToolRequest{})
	if err != nil || decision.Outcome != approval.OutcomeDeny {
		t.Errorf("DenyPolicy.Evaluate = %+v, %v; want deny", decision, err)
	}
}
