package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/store"
)

const (
	healthSessionID     = "aura-health"
	healthEventKind     = "health.transition"
	healthEventSchema   = 1
	healthEventAuthor   = "system-health"
	healthHistoryReplay = 1000
)

type healthEventLog struct {
	events   store.EventStore
	sessions store.SessionService
	mu       sync.Mutex
}

func newHealthEventLog(events store.EventStore, sessions store.SessionService) *healthEventLog {
	return &healthEventLog{events: events, sessions: sessions}
}

func (l *healthEventLog) sink(ctx context.Context, t *health.Transition) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("health event: encode transition: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureSession(ctx); err != nil {
		return err
	}
	sequence, err := l.events.LastSequence(ctx, healthSessionID)
	if err != nil {
		return fmt.Errorf("health event: %w", err)
	}
	eventID, err := newHealthEventID()
	if err != nil {
		return err
	}
	event := store.RuntimeEvent{
		ID:            eventID,
		SessionID:     healthSessionID,
		Sequence:      sequence + 1,
		Author:        healthEventAuthor,
		Kind:          healthEventKind,
		SchemaVersion: healthEventSchema,
		Payload:       payload,
		CreatedAt:     t.At,
	}
	if err := l.events.Append(ctx, &event); err != nil {
		return fmt.Errorf("health event: %w", err)
	}
	return nil
}

func (l *healthEventLog) ensureSession(ctx context.Context) error {
	_, err := l.sessions.Get(ctx, healthSessionID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("health event: %w", err)
	}
	if err := l.sessions.Create(ctx, &store.Session{ID: healthSessionID, OwnerID: "system"}); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
			return nil
		}
		return fmt.Errorf("health event: %w", err)
	}
	return nil
}

func (l *healthEventLog) history(ctx context.Context) ([]health.Transition, error) {
	var transitions []health.Transition
	const pageSize = 1000
	var after uint64
	for {
		listed, err := l.sessions.ListEvents(ctx, healthSessionID, after, pageSize)
		if err != nil {
			if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("health event history: %w", err)
		}
		for i := range listed {
			event := &listed[i]
			if event.Sequence > after {
				after = event.Sequence
			}
			if event.Kind != healthEventKind {
				continue
			}
			var t health.Transition
			if err := json.Unmarshal(event.Payload, &t); err != nil {
				return nil, fmt.Errorf("health event history: decode %s: %w", event.ID, err)
			}
			transitions = append(transitions, t)
		}
		if len(listed) < pageSize {
			return transitions, nil
		}
	}
}

func newHealthEventID() (string, error) {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("health event id: %w", err)
	}
	return "he-" + hex.EncodeToString(data[:]), nil
}
