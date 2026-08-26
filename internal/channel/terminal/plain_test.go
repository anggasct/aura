package terminal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlainRendererSanitizesUntrustedText(t *testing.T) {
	payload, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: "hello\x1b[31m red\x1b[0m\x1b]8;;https://example.test\a link\x1b]8;;\a\x00\xff"})
	if err != nil {
		t.Fatal(err)
	}
	assistant, diagnostics, terminal := (PlainRenderer{}).RenderTurn([]Event{
		{Kind: "model.delta", Payload: payload},
		{Kind: "tool.started", Author: "tool\x1b[2J\x00"},
		{Kind: "turn.completed"},
	})
	if !terminal {
		t.Fatal("turn is not terminal")
	}
	if strings.ContainsAny(assistant, "\x00\x1b\x7f") || strings.ContainsAny(diagnostics[0], "\x00\x1b\x7f") {
		t.Fatalf("unsanitized output: assistant=%q diagnostics=%q", assistant, diagnostics)
	}
	if !strings.Contains(assistant, "hello red link") || !strings.Contains(assistant, "\ufffd") {
		t.Errorf("assistant = %q, want sanitized text", assistant)
	}
}

func TestPlainRendererBoundsAssistantText(t *testing.T) {
	text := strings.Repeat("a", maxRenderBytes*2)
	payload, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	assistant, _, _ := (PlainRenderer{}).RenderTurn([]Event{{Kind: "model.delta", Payload: payload}})
	if len(assistant) > maxRenderBytes {
		t.Fatalf("assistant length = %d, want <= %d", len(assistant), maxRenderBytes)
	}
}

func TestSanitizeTextForErrors(t *testing.T) {
	got := SanitizeText("bad\x1b]8;;https://example.test\a\x00text")
	if strings.ContainsAny(got, "\x00\x1b\x7f") {
		t.Fatalf("sanitized error = %q", got)
	}
}
