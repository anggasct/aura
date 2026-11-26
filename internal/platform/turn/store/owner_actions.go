package turnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	ownerRekeyEventKind       = "principal.rekey.v1"
	profileOwnerSessionPrefix = "profile-"
)

type AccessAction struct {
	Action    string `json:"action"`
	GrantID   string `json:"grant_id,omitempty"`
	Principal string `json:"principal_id,omitempty"`
	Persona   string `json:"agent_id,omitempty"`
	Tool      string `json:"tool_name,omitempty"`
	Account   string `json:"account,omitempty"`
	Plugin    string `json:"plugin_id,omitempty"`
	Version   string `json:"plugin_version,omitempty"`
	Channel   string `json:"channel,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	At        string `json:"at"`
}

func EnsureOwnerActionSession(ctx context.Context, db *sql.DB, ownerID string) (string, error) {
	sessionID := profileOwnerSessionPrefix + ownerID
	sessions := NewSessionService(db)
	if _, err := sessions.Get(ctx, sessionID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		now := time.Now().UTC()
		if err := sessions.Create(ctx, &turn.Session{
			ID: sessionID, OwnerID: ownerID, CreatedAt: now, UpdatedAt: now, Metadata: json.RawMessage(`{}`),
		}); err != nil {
			if !fault.HasCode(err, ErrorCodeSessionIDConflict) {
				return "", err
			}
		}
	}
	return sessionID, nil
}

type OwnerActionRecorder struct {
	db *sql.DB
}

func NewOwnerActionRecorder(db *sql.DB) *OwnerActionRecorder {
	return &OwnerActionRecorder{db: db}
}

func (r *OwnerActionRecorder) RecordAccessAction(ctx context.Context, action *AccessAction, subject string, at time.Time) error {
	if r == nil || r.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "owner action recorder requires a database")
	}
	if action == nil {
		return fault.New(ErrorCodeInvalidArgument, "access action must not be nil")
	}
	sessionID, err := EnsureOwnerActionSession(ctx, r.db, principal.OwnerID)
	if err != nil {
		return err
	}
	recorded := *action
	recorded.At = at.UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(&recorded)
	if err != nil {
		return fmt.Errorf("turnstore: marshal access owner action: %w", err)
	}
	_, _, err = NewEventStore(r.db).UpsertEvent(ctx, &turn.Event{
		ID:            "access-owner-action-" + recorded.Action + "-" + subject + "-" + recorded.At,
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          principal.AccessOwnerActionEventKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     at.UTC(),
	})
	return err
}

func (r *OwnerActionRecorder) RecordOwnerAction(ctx context.Context, action *principal.OwnerAction, subject string, at time.Time) error {
	if action == nil {
		return fault.New(ErrorCodeInvalidArgument, "owner action must not be nil")
	}
	return r.RecordAccessAction(ctx, &AccessAction{
		Action:    action.Action,
		GrantID:   action.GrantID,
		Principal: action.Principal,
		Persona:   action.Persona,
		Tool:      action.Tool,
		Account:   action.Account,
		Plugin:    action.Plugin,
		Version:   action.Version,
		Channel:   action.Channel,
		ExpiresAt: action.ExpiresAt,
	}, subject, at)
}

func (r *OwnerActionRecorder) RecordOwnerRekey(ctx context.Context, report principal.RekeyReport, at time.Time) error {
	if r == nil || r.db == nil {
		return fault.New(ErrorCodeInvalidArgument, "owner action recorder requires a database")
	}
	at = at.UTC()
	sessionID, err := EnsureOwnerActionSession(ctx, r.db, principal.OwnerID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Sessions             int64  `json:"sessions"`
		MemoryDocuments      int64  `json:"memory_documents"`
		ProfileFacts         int64  `json:"profile_facts"`
		ProfileFactConflicts int64  `json:"profile_fact_conflicts"`
		At                   string `json:"at"`
	}{
		Sessions:             report.Count(principal.RekeyLineSessions),
		MemoryDocuments:      report.Count(principal.RekeyLineMemoryDocuments),
		ProfileFacts:         report.Count(principal.RekeyLineProfileFacts),
		ProfileFactConflicts: report.Count(principal.RekeyLineProfileFactConflicts),
		At:                   at.Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("turnstore: marshal owner rekey: %w", err)
	}
	_, _, err = NewEventStore(r.db).UpsertEvent(ctx, &turn.Event{
		ID:            "principal-rekey-" + at.Format(time.RFC3339Nano),
		SessionID:     sessionID,
		TurnID:        "owner-action",
		InvocationID:  "owner-action",
		Author:        principal.OwnerID,
		Kind:          ownerRekeyEventKind,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     at,
	})
	return err
}
