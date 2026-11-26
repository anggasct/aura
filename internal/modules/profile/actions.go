package profile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const ownerSessionPrefix = "profile-"

type SessionStore interface {
	Get(ctx context.Context, id string) (turn.Session, error)
	Create(ctx context.Context, session *turn.Session) error
}

type EventStore interface {
	UpsertEvent(ctx context.Context, event *turn.Event) (uint64, bool, error)
}

type OwnerActionSink struct {
	sessions SessionStore
	events   EventStore
}

func NewActionSink(sessions SessionStore, events EventStore) (*OwnerActionSink, error) {
	if sessions == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "session store must not be nil")
	}
	if events == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "event store must not be nil")
	}
	return &OwnerActionSink{sessions: sessions, events: events}, nil
}

func (s *OwnerActionSink) RecordOwnerAction(ctx context.Context, action *OwnerAction) error {
	if action == nil {
		return fault.New(ErrorCodeInvalidArgument, "owner action must not be nil")
	}
	sessionID, err := s.ensureSession(ctx, action.OwnerID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Action   string `json:"action"`
		FactID   string `json:"fact_id"`
		Category string `json:"category"`
		Key      string `json:"key"`
		At       string `json:"at"`
	}{
		Action: action.Action, FactID: action.FactID, Category: action.Category,
		Key: action.Key, At: action.At.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("marshal owner action: %w", err)
	}
	_, _, err = s.events.UpsertEvent(ctx, &turn.Event{
		ID:            "profile-owner-action-" + action.Action + "-" + action.FactID + "-" + action.At.UTC().Format(time.RFC3339Nano),
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        action.OwnerID,
		Kind:          "profile.owner_action.v1",
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     action.At.UTC(),
	})
	return err
}

func (s *OwnerActionSink) ensureSession(ctx context.Context, ownerID string) (string, error) {
	sessionID := ownerSessionPrefix + ownerID
	if _, err := s.sessions.Get(ctx, sessionID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		now := time.Now().UTC()
		if err := s.sessions.Create(ctx, &turn.Session{
			ID: sessionID, OwnerID: ownerID, CreatedAt: now, UpdatedAt: now, Metadata: json.RawMessage(`{}`),
		}); err != nil {
			if !fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
				return "", err
			}
		}
	}
	return sessionID, nil
}
