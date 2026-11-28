package compaction

import (
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/platform/turn"
)

type eventFunctionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type eventFunctionResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type eventContentPart struct {
	Text             string                 `json:"text"`
	FunctionCall     *eventFunctionCall     `json:"functionCall"`
	FunctionResponse *eventFunctionResponse `json:"functionResponse"`
	InlineData       *struct {
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
			if part.FunctionCall != nil {
				if len(part.FunctionCall.Args) > 0 {
					texts = append(texts, part.FunctionCall.Name+string(part.FunctionCall.Args))
				} else if part.FunctionCall.Name != "" {
					texts = append(texts, part.FunctionCall.Name)
				}
			}
			if part.FunctionResponse != nil {
				event.ToolResult = true
				if len(part.FunctionResponse.Response) > 0 {
					texts = append(texts, string(part.FunctionResponse.Response))
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

type rowContentPart struct {
	PartIndex          int
	IsFunctionResponse bool
	Text               string
}

func extractRowContentParts(raw json.RawMessage) []rowContentPart {
	if len(raw) == 0 {
		return nil
	}
	var payload eventPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Content == nil {
		return nil
	}
	var parts []rowContentPart
	for i, part := range payload.Content.Parts {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, rowContentPart{PartIndex: i, Text: part.Text})
		}
		if part.FunctionCall != nil {
			if len(part.FunctionCall.Args) > 0 {
				parts = append(parts, rowContentPart{PartIndex: i, Text: part.FunctionCall.Name + string(part.FunctionCall.Args)})
			} else if part.FunctionCall.Name != "" {
				parts = append(parts, rowContentPart{PartIndex: i, Text: part.FunctionCall.Name})
			}
		}
		if part.FunctionResponse != nil && len(part.FunctionResponse.Response) > 0 {
			parts = append(parts, rowContentPart{
				PartIndex:          i,
				IsFunctionResponse: true,
				Text:               string(part.FunctionResponse.Response),
			})
		}
	}
	return parts
}
