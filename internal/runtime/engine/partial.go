package runtimeengine

import (
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

func partialModelText(ev *store.RuntimeEvent) (text string, partial bool) {
	if ev.Kind != store.EventKindADK {
		return "", false
	}
	var decoded struct {
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil || !decoded.Partial {
		return "", false
	}
	return adkModelText(ev.Payload, true), true
}

func (e *Engine) deltaFromPartial(t *turn, next *uint64, ev *store.RuntimeEvent) (delta *store.RuntimeEvent, partial bool, err error) {
	text, partial := partialModelText(ev)
	if !partial || text == "" {
		return nil, partial, nil
	}
	payload, err := runtime.EncodeModelDelta(runtime.ModelDelta{Text: text, Index: *next})
	if err != nil {
		return nil, true, codedError(runtime.ErrorCodeRuntimeInternal, "failed to encode model delta", err)
	}
	*next++
	return &store.RuntimeEvent{
		ID:            NewTurnID(),
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        ev.Author,
		Kind:          runtime.EventKindModelDelta,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}, true, nil
}
