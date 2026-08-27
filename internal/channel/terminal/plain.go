package terminal

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxRenderBytes     = 1 << 20
	maxDiagnosticLines = 1024
)

type adkRenderEvent struct {
	Content *struct {
		Role  string `json:"role"`
		Parts []struct {
			Text         *string `json:"text"`
			FunctionCall *struct {
				Name string `json:"name"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				Name string `json:"name"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"content"`
	Actions *struct {
		TransferToAgent string `json:"transferToAgent"`
		Escalate        bool   `json:"escalate"`
	} `json:"actions"`
	LongRunningToolIDs []string `json:"longRunningToolIds"`
	Partial            bool     `json:"partial"`
}

func decodeADKEvent(payload json.RawMessage) (*adkRenderEvent, bool) {
	if len(payload) == 0 || len(payload) > 4*maxRenderBytes {
		return nil, false
	}
	var event adkRenderEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, false
	}
	return &event, true
}

func adkText(event *adkRenderEvent) (text []byte, present, emptyAssistant bool) {
	if event == nil || event.Content == nil || event.Content.Role == "user" {
		return nil, false, false
	}
	hasToolPart := false
	for _, part := range event.Content.Parts {
		if part.Text != nil {
			text = appendLimited(text, []byte(*part.Text))
			present = true
		}
		if part.FunctionCall != nil || part.FunctionResponse != nil {
			hasToolPart = true
		}
	}
	emptyAssistant = !present && !hasToolPart && len(event.Content.Parts) == 0
	return text, present, emptyAssistant
}

// PlainRenderer is the non-TTY event renderer. It folds a turn's events into
// the completed assistant text for stdout and diagnostics for stderr; tool
// and approval activity is surfaced as progress lines, never as model text.
// It emits no ANSI.
type PlainRenderer struct{}

// RenderTurn reports the completed assistant text, any diagnostics to print
// to stderr, and whether the turn reached a durable terminal event.
func (PlainRenderer) RenderTurn(stream []Event) (assistant string, diagnostics []string, terminal bool) {
	var buf []byte
	var finalSet bool
	for _, ev := range stream {
		switch ev.Kind {
		case "turn.completed":
			terminal = true
		case "turn.cancelled":
			terminal = true
			diagnostics = appendDiagnostic(diagnostics, "turn cancelled")
		case "turn.failed":
			terminal = true
			diagnostics = appendDiagnostic(diagnostics, "turn failed")
		case "model.delta":
			if !finalSet {
				buf = appendLimited(buf, decodeDelta(ev.Payload))
			}
		case "message.completed":
			finalSet = true
			buf = appendLimited(nil, decodeDelta(ev.Payload))
		case "adk_event":
			// Batch streams carry the normalized projection; standalone
			// renderer use may still see the raw provider shape.
			var norm struct {
				Text     string `json:"text"`
				Role     string `json:"role"`
				Partial  bool   `json:"partial"`
				Transfer string `json:"transfer"`
				Escalate bool   `json:"escalate"`
			}
			if err := json.Unmarshal(ev.Payload, &norm); err == nil && (norm.Text != "" || norm.Role != "" || norm.Transfer != "" || norm.Escalate) {
				if norm.Text != "" {
					if norm.Partial {
						if !finalSet {
							buf = appendLimited(buf, []byte(norm.Text))
						}
					} else {
						buf = appendLimited(nil, []byte(norm.Text))
						finalSet = true
					}
				}
				if norm.Transfer != "" {
					diagnostics = appendDiagnostic(diagnostics, "agent transfer requested: "+norm.Transfer)
				}
				if norm.Escalate {
					diagnostics = appendDiagnostic(diagnostics, "agent escalation requested")
				}
				continue
			}
			adk, ok := decodeADKEvent(ev.Payload)
			if !ok {
				continue
			}
			text, present, emptyAssistant := adkText(adk)
			if present {
				if adk.Partial {
					if !finalSet {
						buf = appendLimited(buf, text)
					}
				} else {
					buf = appendLimited(nil, text)
					finalSet = true
				}
			} else if emptyAssistant && !adk.Partial {
				buf = nil
				finalSet = true
			}
		case "tool.started":
			diagnostics = appendDiagnostic(diagnostics, "tool started: "+sanitizeText(ev.Author))
		case "tool.completed":
			diagnostics = appendDiagnostic(diagnostics, "tool completed: "+sanitizeText(ev.Author))
		case "approval.required":
			diagnostics = appendDiagnostic(diagnostics, "approval required but denied in plain mode")
		}
	}
	return sanitizeText(string(buf)), diagnostics, terminal
}

// SanitizeText removes terminal control sequences and invalid UTF-8 from user-visible text.
func SanitizeText(text string) string {
	return sanitizeText(text)
}

// decodeDelta extracts model text from a delta or completed-message payload.
// The runtime's canonical text parts live in an adk content payload; fall
// back to a plain "text" field for scripted events.
func decodeDelta(payload json.RawMessage) []byte {
	if len(payload) == 0 || len(payload) > 4*maxRenderBytes {
		return nil
	}
	var content struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &content); err == nil && content.Content != nil {
		var out []byte
		for _, p := range content.Content.Parts {
			out = appendLimited(out, []byte(p.Text))
		}
		return out
	}
	var plain struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &plain); err == nil {
		return []byte(plain.Text)
	}
	return nil
}

func appendDiagnostic(diagnostics []string, text string) []string {
	if len(diagnostics) >= maxDiagnosticLines {
		return diagnostics
	}
	return append(diagnostics, limitText(sanitizeText(text)))
}

func appendLimited(dst, src []byte) []byte {
	if len(dst) >= maxRenderBytes {
		return dst[:maxRenderBytes]
	}
	remaining := maxRenderBytes - len(dst)
	if len(src) > remaining {
		src = src[:remaining]
	}
	return append(dst, src...)
}

func limitText(text string) string {
	if len(text) <= maxRenderBytes {
		return text
	}
	cut := maxRenderBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func sanitizeText(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	var out strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '\x1b':
			i = skipEscape(text, i+size)
		case r == '\x9b':
			i = skipCSI(text, i+size)
		case r == '\x9d':
			i = skipOSC(text, i+size)
		case r == '\n':
			out.WriteRune(r)
			i += size
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			i += size
		default:
			out.WriteRune(r)
			i += size
		}
	}
	return limitText(out.String())
}

func skipEscape(text string, i int) int {
	if i >= len(text) {
		return i
	}
	switch text[i] {
	case '[':
		return skipCSI(text, i+1)
	case ']':
		return skipOSC(text, i+1)
	default:
		for i < len(text) {
			b := text[i]
			i++
			if b >= 0x30 && b <= 0x7e {
				return i
			}
		}
	}
	return i
}

func skipCSI(text string, i int) int {
	for i < len(text) {
		b := text[i]
		i++
		if b >= 0x40 && b <= 0x7e {
			return i
		}
	}
	return i
}

func skipOSC(text string, i int) int {
	for i < len(text) {
		if text[i] == '\a' {
			return i + 1
		}
		if text[i] == '\x1b' && i+1 < len(text) && text[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return i
}
