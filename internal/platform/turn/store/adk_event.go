package turnstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

const EventKindADK = "adk_event"

const adkEventSchemaVersion uint16 = 1

type adkEventPayload struct {
	Content            *genai.Content       `json:"content,omitempty"`
	Actions            session.EventActions `json:"actions"`
	LongRunningToolIDs []string             `json:"longRunningToolIds,omitempty"`
	Partial            bool                 `json:"partial,omitempty"`
	FinishReason       genai.FinishReason   `json:"finishReason,omitempty"`
}

func RuntimeEventFromADK(sessionID, turnID string, ev *session.Event) (turn.Event, error) {
	if ev == nil {
		return turn.Event{}, errNilArgument("event")
	}
	payloadJSON, err := json.Marshal(adkEventPayload{
		Content:            ev.Content,
		Actions:            ev.Actions,
		LongRunningToolIDs: ev.LongRunningToolIDs,
		Partial:            ev.Partial,
		FinishReason:       ev.FinishReason,
	})
	if err != nil {
		return turn.Event{}, fmt.Errorf("marshal adk event %s payload: %w", ev.ID, err)
	}

	var usageJSON json.RawMessage
	if ev.UsageMetadata != nil {
		usageJSON, err = json.Marshal(ev.UsageMetadata)
		if err != nil {
			return turn.Event{}, fmt.Errorf("marshal adk event %s usage: %w", ev.ID, err)
		}
	}

	return turn.Event{
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

func RuntimeEventToADK(e *turn.Event) (*session.Event, error) {
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
			FinishReason:  payload.FinishReason,
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

func ReplayTurn(ctx context.Context, db *sql.DB, sessionID, turnID string) (event *session.Event, found bool, err error) {
	rows, err := sqlite.Conn(ctx, db).QueryContext(ctx,
		`SELECT `+selectRuntimeEventColumns+`
		 FROM runtime_event
		 WHERE session_id = ? AND turn_id = ?
		 ORDER BY sequence ASC`,
		sessionID, turnID,
	)
	if err != nil {
		return nil, false, classify(fmt.Errorf("replay turn %s: %w", turnID, err))
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
		return nil, false, classify(fmt.Errorf("replay turn %s: %w", turnID, err))
	}
	return terminal, terminal != nil, nil
}

func ModelText(payload json.RawMessage, includePartial bool) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded struct {
		Content *struct {
			Role  string `json:"role"`
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	if decoded.Content == nil {
		return ""
	}
	if decoded.Content.Role == "user" {
		return ""
	}
	if decoded.Partial && !includePartial {
		return ""
	}
	var out strings.Builder
	for _, part := range decoded.Content.Parts {
		if part.Text == nil {
			continue
		}
		out.WriteString(*part.Text)
	}
	return out.String()
}

func HasToolActivity(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var decoded struct {
		Content *struct {
			Parts []struct {
				FunctionCall     *struct{} `json:"functionCall"`
				FunctionResponse *struct{} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"content"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return false
	}
	if decoded.Content != nil {
		for _, part := range decoded.Content.Parts {
			if part.FunctionCall != nil || part.FunctionResponse != nil {
				return true
			}
		}
	}
	return false
}
