package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
)

type SessionStore interface {
	Get(ctx context.Context, sessionID string) (turn.Session, error)
	Create(ctx context.Context, sess *turn.Session) error
}

type SessionEnsurer struct {
	sessions SessionStore
}

func NewSessionEnsurer(sessions SessionStore) (*SessionEnsurer, error) {
	if sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "session store must not be nil")
	}
	return &SessionEnsurer{sessions: sessions}, nil
}

func (s *SessionEnsurer) SessionExists(ctx context.Context, sessionID string) (bool, error) {
	if s == nil || s.sessions == nil {
		return false, nil
	}
	_, err := s.sessions.Get(ctx, sessionID)
	if err == nil {
		return true, nil
	}
	if missingSession(err) {
		return false, nil
	}
	return false, err
}

func (s *SessionEnsurer) EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error {
	if s == nil || s.sessions == nil {
		return nil
	}
	if _, err := s.sessions.Get(ctx, sessionID); err == nil {
		return nil
	} else if !missingSession(err) {
		return err
	}
	now := time.Now().UTC()
	err := s.sessions.Create(ctx, &turn.Session{
		ID:        sessionID,
		OwnerID:   ownerID,
		AgentID:   agentID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
		return nil
	}
	return err
}

func (s *SessionEnsurer) SessionOwner(ctx context.Context, sessionID string) (ownerID string, found bool, err error) {
	if s == nil || s.sessions == nil {
		return "", false, nil
	}
	session, err := s.sessions.Get(ctx, sessionID)
	if err == nil {
		return session.OwnerID, true, nil
	}
	if missingSession(err) {
		return "", false, nil
	}
	return "", false, err
}

func missingSession(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || fault.HasCode(err, turnstore.ErrorCodeSessionNotFound)
}

type ConversationStore interface {
	Resolve(ctx context.Context, conversationKey string) (sessionID string, found bool, err error)
}

type ConversationResolver struct {
	pointers ConversationStore
}

func NewConversationResolver(pointers ConversationStore) (*ConversationResolver, error) {
	if pointers == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "conversation store must not be nil")
	}
	return &ConversationResolver{pointers: pointers}, nil
}

func (r *ConversationResolver) ActiveSession(ctx context.Context, conversationKey string) (string, error) {
	if r == nil || r.pointers == nil {
		return conversationKey, nil
	}
	id, found, err := r.pointers.Resolve(ctx, conversationKey)
	if err != nil {
		return "", err
	}
	if !found {
		return conversationKey, nil
	}
	return id, nil
}
