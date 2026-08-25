package terminal

import "encoding/json"

// PlainRenderer is the non-TTY event renderer. It folds a turn's events into
// the completed assistant text for stdout and diagnostics for stderr; tool
// and approval activity is surfaced as progress lines, never as model text.
// It emits no ANSI.
type PlainRenderer struct{}

// RenderTurn reports the completed assistant text, any diagnostics to print
// to stderr, and whether the turn reached a durable terminal event.
func (PlainRenderer) RenderTurn(stream []Event) (assistant string, diagnostics []string, terminal bool) {
	var buf []byte
	for _, ev := range stream {
		switch ev.Kind {
		case "turn.completed":
			terminal = true
		case "turn.cancelled":
			terminal = true
			diagnostics = append(diagnostics, "turn cancelled")
		case "turn.failed":
			terminal = true
			diagnostics = append(diagnostics, "turn failed")
		case "model.delta":
			buf = append(buf, decodeDelta(ev.Payload)...)
		case "message.completed":
			if text := decodeDelta(ev.Payload); len(text) > 0 {
				buf = append(buf[:0], text...)
			}
		case "tool.started":
			diagnostics = append(diagnostics, "tool started: "+ev.Author)
		case "tool.completed":
			diagnostics = append(diagnostics, "tool completed: "+ev.Author)
		case "approval.required":
			diagnostics = append(diagnostics, "approval required but denied in plain mode")
		}
	}
	return string(buf), diagnostics, terminal
}

// decodeDelta extracts model text from a delta or completed-message payload.
// The runtime's canonical text parts live in an adk content payload; fall
// back to a plain "text" field for scripted events.
func decodeDelta(payload json.RawMessage) []byte {
	if len(payload) == 0 {
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
			out = append(out, p.Text...)
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
