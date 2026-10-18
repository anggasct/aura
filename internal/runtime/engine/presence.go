package runtimeengine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/anggasct/aura/internal/runtime"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
)

func presenceDeliveryRequest(req *runtime.TurnRequest) *runtimechannelhost.DeliveryRequest {
	if req == nil {
		return nil
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       req.TurnID,
		Channel:        string(req.Origin),
		ConversationID: req.SessionID,
		ReplyContext:   req.ReplyContext,
		IdempotencyKey: req.TurnID,
		Deadline:       req.Deadline,
	}
}

func presenceAdapterFor(req *runtime.TurnRequest, adapters []runtimechannelhost.ChannelPort) (runtimechannelhost.PresencePort, *runtimechannelhost.DeliveryRequest, bool) {
	if req == nil || req.TurnID == "" {
		return nil, nil, false
	}
	if len(adapters) == 0 {
		return nil, nil, false
	}
	delivery := presenceDeliveryRequest(req)
	if delivery == nil {
		return nil, nil, false
	}
	for _, adapter := range adapters {
		if presence, ok := adapter.(runtimechannelhost.PresencePort); ok {
			return presence, delivery, true
		}
	}
	return nil, nil, false
}

func (e *Engine) currentPresence(req *runtime.TurnRequest) (runtimechannelhost.PresencePort, *runtimechannelhost.DeliveryRequest, bool) {
	e.mu.Lock()
	adapters := e.adapters
	e.mu.Unlock()
	return presenceAdapterFor(req, adapters)
}

func extractPresenceText(ev *store.RuntimeEvent) (string, bool) {
	if ev == nil {
		return "", false
	}
	if ev.Kind != store.EventKindADK {
		return "", false
	}
	text := adkModelText(ev.Payload, false)
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func isPresenceFailureEvent(ev *store.RuntimeEvent) bool {
	if ev == nil || ev.Kind != store.EventKindADK {
		return false
	}
	var decoded struct {
		Content *struct {
			Parts []struct {
				FunctionResponse *struct {
					Name     string         `json:"name"`
					Response map[string]any `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil || decoded.Content == nil {
		return false
	}
	for _, part := range decoded.Content.Parts {
		if part.FunctionResponse == nil {
			continue
		}
		for key, value := range part.FunctionResponse.Response {
			if !strings.EqualFold(key, "error") {
				continue
			}
			switch v := value.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return true
				}
			case nil:
			default:
				return true
			}
		}
	}
	return false
}

func (e *Engine) notifyPresenceStarted(ctx context.Context, req *runtime.TurnRequest) {
	presence, delivery, ok := e.currentPresence(req)
	if !ok {
		return
	}
	if err := presence.PresenceStarted(ctx, delivery); err != nil {
		e.logger.DebugContext(ctx, "presence start failed", "error", err, "turn_id", req.TurnID)
	}
}

func (e *Engine) notifyPresenceText(ctx context.Context, req *runtime.TurnRequest, text string) {
	presence, delivery, ok := e.currentPresence(req)
	if !ok {
		return
	}
	if err := presence.PresenceUpdated(ctx, delivery, text, false); err != nil {
		e.logger.DebugContext(ctx, "presence update failed", "error", err, "turn_id", req.TurnID)
	}
}

func (e *Engine) notifyPresenceFailed(ctx context.Context, req *runtime.TurnRequest) {
	presence, delivery, ok := e.currentPresence(req)
	if !ok {
		return
	}
	if err := presence.PresenceUpdated(ctx, delivery, "", true); err != nil {
		e.logger.DebugContext(ctx, "presence failure update failed", "error", err, "turn_id", req.TurnID)
	}
}

func (e *Engine) notifyPresenceEnded(ctx context.Context, req *runtime.TurnRequest) {
	presence, _, ok := e.currentPresence(req)
	if !ok {
		return
	}
	if err := presence.PresenceEnded(context.WithoutCancel(ctx), req.TurnID); err != nil {
		e.logger.DebugContext(ctx, "presence end failed", "error", err, "turn_id", req.TurnID)
	}
}
