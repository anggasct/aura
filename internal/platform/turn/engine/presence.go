package runtimeengine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

func presenceDeliveryRequest(req *turn.TurnRequest, deadline time.Time) *runtimechannelhost.DeliveryRequest {
	if req == nil {
		return nil
	}
	if deadline.IsZero() {
		deadline = req.Deadline
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       req.TurnID,
		Channel:        string(req.Origin),
		ConversationID: req.SessionID,
		AgentID:        req.AgentID,
		PrincipalClass: string(req.PrincipalClass),
		ReplyContext:   req.ReplyContext,
		IdempotencyKey: req.TurnID,
		Deadline:       deadline,
		Tainted:        req.Tainted(),
	}
}

func presenceAdapterFor(req *turn.TurnRequest, deadline time.Time, adapters []runtimechannelhost.ChannelPort) (runtimechannelhost.PresencePort, *runtimechannelhost.DeliveryRequest, bool) {
	if req == nil || req.TurnID == "" || !hasChannelDestination(req) {
		return nil, nil, false
	}
	if len(adapters) == 0 {
		return nil, nil, false
	}
	delivery := presenceDeliveryRequest(req, deadline)
	if delivery == nil {
		return nil, nil, false
	}
	presenceCapable := make([]runtimechannelhost.ChannelPort, 0, len(adapters))
	for _, adapter := range adapters {
		if _, ok := adapter.(runtimechannelhost.PresencePort); ok {
			presenceCapable = append(presenceCapable, adapter)
		}
	}
	adapter, ok := adapterForOrigin(req.Origin, presenceCapable)
	if !ok {
		return nil, nil, false
	}
	presence, ok := adapter.(runtimechannelhost.PresencePort)
	return presence, delivery, ok
}

func adapterForOrigin(origin turn.Origin, adapters []runtimechannelhost.ChannelPort) (runtimechannelhost.ChannelPort, bool) {
	var fallback runtimechannelhost.ChannelPort
	for _, adapter := range adapters {
		identity, named := adapter.(runtimechannelhost.ChannelIdentity)
		if named && identity.ChannelName() != "" {
			if origin.OnChannel(identity.ChannelName()) {
				return adapter, true
			}
			continue
		}
		if fallback == nil {
			fallback = adapter
		}
	}
	return fallback, fallback != nil
}

func (e *Engine) currentPresence(req *turn.TurnRequest, deadline time.Time) (runtimechannelhost.PresencePort, *runtimechannelhost.DeliveryRequest, bool) {
	return presenceAdapterFor(req, deadline, e.adapters)
}

type functionCall struct {
	ID string `json:"id"`
}

type functionResponse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

func functionResponses(ev *turn.Event) []functionResponse {
	if ev == nil || ev.Kind != turnstore.EventKindADK {
		return nil
	}
	var decoded struct {
		Content *struct {
			Parts []struct {
				FunctionResponse *functionResponse `json:"functionResponse"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil || decoded.Content == nil {
		return nil
	}
	var responses []functionResponse
	for _, part := range decoded.Content.Parts {
		if part.FunctionResponse != nil {
			responses = append(responses, *part.FunctionResponse)
		}
	}
	return responses
}

type presenceStep struct {
	callIDs  []string
	activity string
	pending  map[string]struct{}
	settled  bool
}

type presenceTracker struct {
	byCallID map[string]*presenceStep
}

func (t *presenceTracker) observe(ev *turn.Event) []runtimechannelhost.PresenceUpdate {
	if ev == nil || ev.Kind != turnstore.EventKindADK || len(ev.Payload) == 0 {
		return nil
	}
	var decoded struct {
		Content *struct {
			Parts []struct {
				FunctionCall     *functionCall     `json:"functionCall"`
				FunctionResponse *functionResponse `json:"functionResponse"`
			} `json:"parts"`
		} `json:"content"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(ev.Payload, &decoded); err != nil || decoded.Partial || decoded.Content == nil {
		return nil
	}
	hasCalls := false
	var callIDs []string
	var pending map[string]struct{}
	var responses []functionResponse
	for _, part := range decoded.Content.Parts {
		if part.FunctionCall != nil {
			hasCalls = true
			if id := strings.TrimSpace(part.FunctionCall.ID); id != "" {
				if pending == nil {
					pending = map[string]struct{}{}
				}
				if _, seen := pending[id]; !seen {
					pending[id] = struct{}{}
					callIDs = append(callIDs, id)
				}
			}
		}
		if part.FunctionResponse != nil {
			responses = append(responses, *part.FunctionResponse)
		}
	}
	var updates []runtimechannelhost.PresenceUpdate
	if hasCalls {
		step := &presenceStep{
			callIDs:  callIDs,
			activity: turnstore.ModelText(ev.Payload, false),
			pending:  pending,
		}
		if len(callIDs) > 0 {
			if t.byCallID == nil {
				t.byCallID = map[string]*presenceStep{}
			}
			for _, id := range callIDs {
				t.byCallID[id] = step
			}
		}
		updates = append(updates, runtimechannelhost.PresenceUpdate{
			CallIDs:  slices.Clone(step.callIDs),
			Activity: step.activity,
			Outcome:  runtimechannelhost.StepRunning,
		})
	}
	for _, response := range responses {
		id := strings.TrimSpace(response.ID)
		if id == "" {
			continue
		}
		step := t.byCallID[id]
		if step == nil || step.settled {
			continue
		}
		if _, ok := step.pending[id]; !ok {
			continue
		}
		delete(step.pending, id)
		if turn.IsToolError(response.Response) {
			step.settled = true
			updates = append(updates, runtimechannelhost.PresenceUpdate{
				CallIDs:  slices.Clone(step.callIDs),
				Activity: step.activity,
				Outcome:  runtimechannelhost.StepFailed,
			})
			continue
		}
		if len(step.pending) == 0 {
			step.settled = true
			updates = append(updates, runtimechannelhost.PresenceUpdate{
				CallIDs:  slices.Clone(step.callIDs),
				Activity: step.activity,
				Outcome:  runtimechannelhost.StepDone,
			})
		}
	}
	return updates
}

func (e *Engine) notifyPresenceStarted(ctx context.Context, req *turn.TurnRequest, deadline time.Time) {
	presence, delivery, ok := e.currentPresence(req, deadline)
	if !ok {
		return
	}
	if err := presence.PresenceStarted(ctx, delivery); err != nil {
		e.logger.DebugContext(ctx, "presence start failed", "error", err, "turn_id", req.TurnID)
	}
}

func (e *Engine) notifyPresenceEvent(ctx context.Context, req *turn.TurnRequest, deadline time.Time, tracker *presenceTracker, ev *turn.Event) {
	updates := tracker.observe(ev)
	if len(updates) == 0 {
		return
	}
	presence, delivery, ok := e.currentPresence(req, deadline)
	if !ok {
		return
	}
	for _, update := range updates {
		if err := presence.PresenceUpdated(ctx, delivery, update); err != nil {
			e.logger.DebugContext(ctx, "presence update failed", "error", err, "turn_id", req.TurnID)
		}
	}
}

func (e *Engine) notifyPresenceEnded(ctx context.Context, req *turn.TurnRequest, end runtimechannelhost.PresenceEnd) {
	presence, _, ok := e.currentPresence(req, time.Time{})
	if !ok {
		return
	}
	if err := presence.PresenceEnded(context.WithoutCancel(ctx), req.TurnID, end); err != nil {
		e.logger.DebugContext(ctx, "presence end failed", "error", err, "turn_id", req.TurnID)
	}
}
