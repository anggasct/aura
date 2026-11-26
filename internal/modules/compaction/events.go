package compaction

import (
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/platform/turn"
)

type eventContentPart struct {
	Text             string `json:"text"`
	FunctionResponse *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"functionResponse"`
	InlineData *struct {
		MIMEType string `json:"mimeType"`
	} `json:"inlineData"`
	FileData *struct {
		MIMEType string `json:"mimeType"`
	} `json:"fileData"`
}

type eventPayload struct {
	Content *struct {
		Parts []eventContentPart `json:"parts"`
	} `json:"content"`
	Text           string `json:"text"`
	Partial        bool   `json:"partial"`
	ToolCallID     string `json:"tool_call_id"`
	EffectIntentID string `json:"effect_intent_id"`
	ApprovalID     string `json:"approval_id"`
}

func MapEvent(row *turn.Event) Event {
	if row == nil {
		return Event{}
	}
	event := Event{
		ID:           row.ID,
		Sequence:     row.Sequence,
		TurnID:       row.TurnID,
		InvocationID: row.InvocationID,
		Kind:         row.Kind,
	}
	var payload eventPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return event
	}
	if payload.Partial {
		return event
	}
	var texts []string
	if payload.Content != nil {
		for _, part := range payload.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				texts = append(texts, part.Text)
			}
			if part.FunctionResponse != nil {
				event.ToolResult = true
				if event.CorrelationID == "" {
					event.CorrelationID = part.FunctionResponse.ID
				}
			}
			if event.MediaType == "" {
				switch {
				case part.InlineData != nil && strings.TrimSpace(part.InlineData.MIMEType) != "":
					event.MediaType = part.InlineData.MIMEType
				case part.FileData != nil && strings.TrimSpace(part.FileData.MIMEType) != "":
					event.MediaType = part.FileData.MIMEType
				}
			}
		}
	}
	if len(texts) == 0 && strings.TrimSpace(payload.Text) != "" {
		texts = append(texts, payload.Text)
	}
	event.Text = strings.Join(texts, "")
	for _, candidate := range []string{payload.ToolCallID, payload.EffectIntentID, payload.ApprovalID} {
		if strings.TrimSpace(candidate) != "" {
			event.CorrelationID = candidate
			break
		}
	}
	if row.Kind == "tool.completed" {
		event.ToolResult = true
	}
	return event
}
