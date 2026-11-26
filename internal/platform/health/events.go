package health

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const (
	EventSessionID     = "aura-health"
	EventKind          = "health.transition"
	EventSchemaVersion = 1
	EventAuthor        = "system-health"
	eventHistoryPage   = 1000
)

type StoredEvent struct {
	ID       string
	Sequence uint64
	Kind     string
	Payload  json.RawMessage
}

type EventRecord struct {
	ID            string
	SessionID     string
	Sequence      uint64
	Author        string
	Kind          string
	SchemaVersion uint16
	Payload       json.RawMessage
	CreatedAt     time.Time
}

type EventAppender interface {
	LastSequence(ctx context.Context, sessionID string) (uint64, error)
	Append(ctx context.Context, event *EventRecord) error
}

type SessionEnsurer interface {
	EnsureSystemSession(ctx context.Context, sessionID, ownerID string) error
	ListSessionEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) (events []StoredEvent, found bool, err error)
}

type EventLog struct {
	events   EventAppender
	sessions SessionEnsurer
	mu       sync.Mutex
}

func NewEventLog(events EventAppender, sessions SessionEnsurer) *EventLog {
	return &EventLog{events: events, sessions: sessions}
}

func (l *EventLog) Sink(ctx context.Context, t *Transition) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("health event: encode transition: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.sessions.EnsureSystemSession(ctx, EventSessionID, "system"); err != nil {
		return fmt.Errorf("health event: %w", err)
	}
	sequence, err := l.events.LastSequence(ctx, EventSessionID)
	if err != nil {
		return fmt.Errorf("health event: %w", err)
	}
	eventID, err := newHealthEventID()
	if err != nil {
		return err
	}
	event := EventRecord{
		ID:            eventID,
		SessionID:     EventSessionID,
		Sequence:      sequence + 1,
		Author:        EventAuthor,
		Kind:          EventKind,
		SchemaVersion: EventSchemaVersion,
		Payload:       payload,
		CreatedAt:     t.At,
	}
	if err := l.events.Append(ctx, &event); err != nil {
		return fmt.Errorf("health event: %w", err)
	}
	return nil
}

func (l *EventLog) History(ctx context.Context) ([]Transition, error) {
	var transitions []Transition
	var after uint64
	for {
		listed, found, err := l.sessions.ListSessionEvents(ctx, EventSessionID, after, eventHistoryPage)
		if err != nil {
			return nil, fmt.Errorf("health event history: %w", err)
		}
		if !found {
			return nil, nil
		}
		for i := range listed {
			event := &listed[i]
			if event.Sequence > after {
				after = event.Sequence
			}
			if event.Kind != EventKind {
				continue
			}
			var t Transition
			if err := json.Unmarshal(event.Payload, &t); err != nil {
				return nil, fmt.Errorf("health event history: decode %s: %w", event.ID, err)
			}
			transitions = append(transitions, t)
		}
		if len(listed) < eventHistoryPage {
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
