package runtimeengine

import (
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

func partialModelText(ev *turn.Event) (text string, partial bool) {
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

func (e *Engine) deltaFromPartial(t *turnState, next *uint64, ev *turn.Event) (delta *turn.Event, partial bool, err error) {
	text, partial := partialModelText(ev)
	if !partial || text == "" {
		return nil, partial, nil
	}
	payload, err := turn.EncodeModelDelta(turn.ModelDelta{Text: text, Index: *next})
	if err != nil {
		return nil, true, codedError(turn.ErrorCodeRuntimeInternal, "failed to encode model delta", err)
	}
	*next++
	return &turn.Event{
		ID:            NewTurnID(),
		SessionID:     t.req.SessionID,
		TurnID:        t.turnID,
		Author:        ev.Author,
		Kind:          turn.EventKindModelDelta,
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}, true, nil
}
