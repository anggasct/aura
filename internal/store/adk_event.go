package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// EventKindADK marks a RuntimeEvent whose Payload is an adkEventPayload
// produced by an ADK agent turn.
const EventKindADK = "adk_event"

const adkEventSchemaVersion uint16 = 1

type adkEventPayload struct {
	Content            *genai.Content       `json:"content,omitempty"`
	Actions            session.EventActions `json:"actions"`
	LongRunningToolIDs []string             `json:"longRunningToolIds,omitempty"`
	// Partial must round-trip alongside Content: session.Event.IsFinalResponse
	// checks both, so dropping it would make every replayed event look final.
	Partial bool `json:"partial,omitempty"`
}

// RuntimeEventFromADK converts an ADK session event into the RuntimeEvent
// shape this package persists, preserving invocation, branch, author,
// actions, content, usage, and long-running tool identifiers. The caller
// still owns Sequence assignment before Append.
func RuntimeEventFromADK(sessionID, turnID string, ev *session.Event) (RuntimeEvent, error) {
	if ev == nil {
		return RuntimeEvent{}, errNilArgument("event")
	}
	payloadJSON, err := json.Marshal(adkEventPayload{
		Content:            ev.Content,
		Actions:            ev.Actions,
		LongRunningToolIDs: ev.LongRunningToolIDs,
		Partial:            ev.Partial,
	})
	if err != nil {
		return RuntimeEvent{}, fmt.Errorf("marshal adk event %s payload: %w", ev.ID, err)
	}

	var usageJSON json.RawMessage
	if ev.UsageMetadata != nil {
		usageJSON, err = json.Marshal(ev.UsageMetadata)
		if err != nil {
			return RuntimeEvent{}, fmt.Errorf("marshal adk event %s usage: %w", ev.ID, err)
		}
	}

	return RuntimeEvent{
		ID:            ev.ID,
		SessionID:     sessionID,
		TurnID:        turnID,
		InvocationID:  ev.InvocationID,
		Branch:        ev.Branch,
		Author:        ev.Author,
		Kind:          EventKindADK,
		SchemaVersion: adkEventSchemaVersion,
		Payload:       payloadJSON,
		ProviderUsage: usageJSON,
		CreatedAt:     ev.Timestamp,
	}, nil
}

// RuntimeEventToADK reverses RuntimeEventFromADK, reconstructing an ADK
// session event from a stored RuntimeEvent.
func RuntimeEventToADK(e *RuntimeEvent) (*session.Event, error) {
	if e == nil {
		return nil, errNilArgument("event")
	}
	if e.Kind != EventKindADK {
		return nil, fmt.Errorf("event %s has kind %q, want %q", e.ID, e.Kind, EventKindADK)
	}

	var payload adkEventPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal adk event %s payload: %w", e.ID, err)
	}

	var usage *genai.GenerateContentResponseUsageMetadata
	if len(e.ProviderUsage) > 0 {
		usage = &genai.GenerateContentResponseUsageMetadata{}
		if err := json.Unmarshal(e.ProviderUsage, usage); err != nil {
			return nil, fmt.Errorf("unmarshal adk event %s usage: %w", e.ID, err)
		}
	}

	return &session.Event{
		LLMResponse: adkmodel.LLMResponse{
			Content:       payload.Content,
			UsageMetadata: usage,
			Partial:       payload.Partial,
		},
		ID:                 e.ID,
		Timestamp:          e.CreatedAt,
		InvocationID:       e.InvocationID,
		Branch:             e.Branch,
		Author:             e.Author,
		Actions:            payload.Actions,
		LongRunningToolIDs: payload.LongRunningToolIDs,
	}, nil
}

// ReplayTurn reconstructs a turn's terminal ADK event from stored runtime
// events alone, with no message projection involved. found is false when
// the turn has not yet produced a final response.
func ReplayTurn(ctx context.Context, db *sql.DB, sessionID, turnID string) (event *session.Event, found bool, err error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+`
		 FROM runtime_event
		 WHERE session_id = ? AND turn_id = ?
		 ORDER BY sequence ASC`,
		sessionID, turnID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("replay turn %s: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()

	var terminal *session.Event
	for rows.Next() {
		e, err := scanRuntimeEvent(rows)
		if err != nil {
			return nil, false, fmt.Errorf("replay turn %s: %w", turnID, err)
		}
		ev, err := RuntimeEventToADK(&e)
		if err != nil {
			return nil, false, fmt.Errorf("replay turn %s: %w", turnID, err)
		}
		if ev.IsFinalResponse() {
			terminal = ev
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("replay turn %s: %w", turnID, err)
	}
	return terminal, terminal != nil, nil
}
