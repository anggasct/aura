package turn

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	legacyEventKindADK           = "adk_event"
	legacyEventKindKBRetrieved   = "kb.retrieved.v1"
	legacyEventKindToolCompleted = "tool.completed"
)

func FirstLegacyUntrustedSequence(ownerID string, events []Event, toolMayTaint func(name string) bool) (uint64, bool) {
	for i := range events {
		if legacyEventTaints(ownerID, &events[i], toolMayTaint) {
			return events[i].Sequence, true
		}
	}
	return 0, false
}

func legacyEventTaints(ownerID string, ev *Event, toolMayTaint func(name string) bool) bool {
	switch ev.Kind {
	case EventKindTurnAccepted:
		return legacyAcceptedTaints(ownerID, ev.Author, ev.Payload)
	case legacyEventKindKBRetrieved:
		return legacyKnowledgeTaints(ev.Payload)
	case legacyEventKindToolCompleted:
		return legacyToolCompletedTaints(ev.Payload, toolMayTaint)
	case legacyEventKindADK:
		return legacyADKEventTaints(ev.Payload, toolMayTaint)
	default:
		return false
	}
}

func legacyAcceptedTaints(ownerID, author string, payload []byte) bool {
	var accepted struct {
		Origin    string `json:"origin"`
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal(payload, &accepted); err != nil {
		return true
	}
	origin := Origin(accepted.Origin)
	if !authoredInputOrigin(origin) || origin == OriginHTTP {
		return true
	}
	actor := accepted.Principal
	if actor == "" {
		actor = author
	}
	if actor == "" || actor == AuthorSystem {
		return origin != OriginSchedule
	}
	if ownerID != "" && actor != ownerID {
		return true
	}
	return principal.ClassForPrincipal(actor) != principal.ClassOwner
}

func legacyKnowledgeTaints(payload []byte) bool {
	var record struct {
		Outcome     string `json:"outcome"`
		OnlyTrusted *bool  `json:"only_trusted"`
		Counts      struct {
			Rendered int `json:"rendered"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		return true
	}
	if record.Outcome != "ok" || record.Counts.Rendered <= 0 {
		return false
	}
	return record.OnlyTrusted == nil || !*record.OnlyTrusted
}

func legacyToolCompletedTaints(payload []byte, toolMayTaint func(name string) bool) bool {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil || raw == nil {
		return true
	}
	name, _ := raw["tool_name"].(string)
	if name == "" {
		name, _ = raw["name"].(string)
	}
	return legacyToolResultTaints(name, raw, toolMayTaint)
}

func legacyADKEventTaints(payload []byte, toolMayTaint func(name string) bool) bool {
	var ev struct {
		Content *struct {
			Parts []struct {
				Text             string          `json:"text"`
				InlineData       json.RawMessage `json:"inlineData"`
				FileData         json.RawMessage `json:"fileData"`
				FunctionResponse *struct {
					Name     string         `json:"name"`
					Response map[string]any `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return true
	}
	if ev.Content == nil {
		return false
	}
	for i := range ev.Content.Parts {
		part := &ev.Content.Parts[i]
		if hasNonNullJSON(part.InlineData) || hasNonNullJSON(part.FileData) {
			return true
		}
		if hasLegacyUntrustedMarker(part.Text) {
			return true
		}
		if part.FunctionResponse != nil && legacyToolResultTaints(part.FunctionResponse.Name, part.FunctionResponse.Response, toolMayTaint) {
			return true
		}
	}
	return false
}

func hasNonNullJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func hasLegacyUntrustedMarker(text string) bool {
	if text == "" {
		return false
	}
	markers := []string{
		"[untrusted recall evidence",
		"[untrusted_knowledge",
		"[untrusted_input",
		"[untrusted_skill_context",
		"[untrusted profile context",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[attachment ") && strings.Contains(trimmed, "artifact:") {
			return true
		}
	}
	return false
}

func legacyToolResultTaints(name string, response map[string]any, toolMayTaint func(name string) bool) bool {
	if name == "" {
		return true
	}
	mayTaint := true
	if toolMayTaint != nil {
		mayTaint = toolMayTaint(name)
	}
	if !mayTaint {
		return false
	}
	if response == nil {
		return true
	}
	return !isLegacyBrokerRefusal(response)
}

func isLegacyBrokerRefusal(response map[string]any) bool {
	status, _ := response["status"].(string)
	switch status {
	case "denied", "approval_denied", "approval_expired", "approval_unreachable", "approval_timeout", "capability_missing", "invalid_arguments", "unknown_tool", "budget_exceeded":
		return true
	}
	if errObj, ok := response["error"].(map[string]any); ok {
		code, _ := errObj["code"].(string)
		switch code {
		case "policy_denied", "approval_denied", "approval_timeout", "approval_unreachable", "invalid_arguments", "unknown_tool", "capability_missing", "rate_limited", "budget_exceeded":
			return true
		}
	}
	return false
}
