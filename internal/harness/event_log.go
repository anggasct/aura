package harness

import (
	"bytes"
	"context"
	"sync"
)

type EventLog struct {
	mu     sync.RWMutex
	events []Event
}

func NewEventLog() *EventLog {
	return &EventLog{}
}

func (l *EventLog) Append(ctx context.Context, event *Event) error {
	if ctx == nil || event == nil {
		return codedError(ErrorCodeInvalidArgument, "event log append requires context and event", nil)
	}
	if err := ctx.Err(); err != nil {
		return codedError(ErrorCodeDurabilityFailed, "event log context is no longer active", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.events {
		existing := &l.events[i]
		if existing.ID == event.ID {
			if existing.SessionID == event.SessionID && existing.TurnID == event.TurnID && bytes.Equal(existing.Payload, event.Payload) {
				return nil
			}
			return codedError(ErrorCodeDurabilityFailed, "event id is already bound to another payload", nil)
		}
	}
	copyEvent := *event
	copyEvent.Payload = bytes.Clone(event.Payload)
	l.events = append(l.events, copyEvent)
	return nil
}

func (l *EventLog) Replay(sessionID, turnID string) []Event {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]Event, 0, len(l.events))
	for i := range l.events {
		event := &l.events[i]
		if event.SessionID != sessionID || event.TurnID != turnID {
			continue
		}
		copyEvent := *event
		copyEvent.Payload = bytes.Clone(event.Payload)
		result = append(result, copyEvent)
	}
	return result
}

func (l *EventLog) Snapshot() []Event {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]Event, 0, len(l.events))
	for i := range l.events {
		event := &l.events[i]
		copyEvent := *event
		copyEvent.Payload = bytes.Clone(event.Payload)
		result = append(result, copyEvent)
	}
	return result
}
