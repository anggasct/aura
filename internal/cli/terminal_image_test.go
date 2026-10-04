package cli

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type captureEngine struct {
	got *runtime.TurnRequest
}

func (c *captureEngine) Run(_ context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	c.got = req
	return func(yield func(store.RuntimeEvent, error) bool) {}
}

func TestTerminalRunnerPreservesImageParts(t *testing.T) {
	engine := &captureEngine{}
	runner := &terminalRunner{engine: engine}
	envelope := json.RawMessage(`{"kind":"image_ref.v1"}`)
	req := &terminal.Request{
		SessionID:   "sess-1",
		PrincipalID: "owner",
		Parts: []terminal.Input{
			{Text: "look"},
			{Image: envelope},
		},
	}
	for range runner.Run(t.Context(), req) {
	}
	if engine.got == nil {
		t.Fatal("engine never received request")
	}
	if len(engine.got.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(engine.got.Parts))
	}
	if engine.got.Parts[0].Text != "look" {
		t.Errorf("text part = %q", engine.got.Parts[0].Text)
	}
	if string(engine.got.Parts[1].Image) != string(envelope) {
		t.Errorf("image envelope lost: %s", engine.got.Parts[1].Image)
	}
}
