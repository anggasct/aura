package runtimeengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/store"
)

func (e *Engine) SetChannelAdapters(adapters []runtimechannelhost.ChannelPort) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.adapters = adapters
	if e.delivered == nil {
		e.delivered = map[string]time.Time{}
	}
}

func (e *Engine) channelAdapterFor(req *turn.TurnRequest) (runtimechannelhost.ChannelPort, bool) {
	if req == nil {
		return nil, false
	}
	switch req.Origin {
	case turn.OriginTerminal, turn.OriginInternal, turn.OriginWebhook, turn.OriginHTTP:
		return nil, false
	}
	if len(req.ReplyContext) == 0 {
		return nil, false
	}
	e.mu.Lock()
	adapters := e.adapters
	e.mu.Unlock()
	return adapterForOrigin(req.Origin, adapters)
}

func (e *Engine) pruneDeliveredLocked(now time.Time) {
	for id, at := range e.delivered {
		if now.Sub(at) >= dedupeWindow {
			delete(e.delivered, id)
		}
	}
}

func (e *Engine) markDelivered(turnID string) bool {
	if turnID == "" {
		return false
	}
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.delivered == nil {
		e.delivered = map[string]time.Time{}
	}
	e.pruneDeliveredLocked(now)
	if at, ok := e.delivered[turnID]; ok {
		if now.Sub(at) < dedupeWindow {
			return false
		}
	}
	e.delivered[turnID] = now
	return true
}

func (e *Engine) unmarkDelivered(turnID string) {
	if turnID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.delivered, turnID)
}

func (e *Engine) hasDelivered(turnID string) bool {
	if turnID == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.delivered[turnID]
	if !ok {
		return false
	}
	if time.Now().UTC().Sub(at) >= dedupeWindow {
		delete(e.delivered, turnID)
		return false
	}
	return true
}

func extractDeliveryTexts(events []store.RuntimeEvent) []string {
	var texts []string
	for i := range events {
		ev := &events[i]
		switch ev.Kind {
		case store.EventKindADK:
			if text := adkAnswerText(ev.Payload, false); strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		case turn.EventKindMessageCompleted:
			for _, text := range messageCompletedTexts(ev.Payload) {
				if strings.TrimSpace(text) != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	if len(texts) == 0 {
		for i := range events {
			ev := &events[i]
			if ev.Kind != store.EventKindADK {
				continue
			}
			if text := adkAnswerText(ev.Payload, true); strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

func adkAnswerText(payload json.RawMessage, includePartial bool) string {
	if hasToolActivity(payload) {
		return ""
	}
	return adkModelText(payload, includePartial)
}

func adkModelText(payload json.RawMessage, includePartial bool) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded struct {
		Content *struct {
			Role  string `json:"role"`
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	if decoded.Content == nil {
		return ""
	}
	if decoded.Content.Role == "user" {
		return ""
	}
	if decoded.Partial && !includePartial {
		return ""
	}
	var out strings.Builder
	for _, part := range decoded.Content.Parts {
		if part.Text == nil {
			continue
		}
		out.WriteString(*part.Text)
	}
	return out.String()
}

func messageCompletedTexts(payload json.RawMessage) []string {
	if len(payload) == 0 {
		return nil
	}
	var plain struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &plain); err == nil && strings.TrimSpace(plain.Text) != "" {
		return []string{plain.Text}
	}
	var shaped struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &shaped); err == nil && shaped.Content != nil {
		var texts []string
		for _, part := range shaped.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) > 0 {
			return texts
		}
	}
	return nil
}

func deliveryParts(texts []string) []runtimechannelhost.OutputPart {
	parts := make([]runtimechannelhost.OutputPart, 0, len(texts))
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, runtimechannelhost.OutputPart{Text: text})
	}
	return parts
}

type plannedDelivery struct {
	adapter runtimechannelhost.ChannelPort
	request *runtimechannelhost.DeliveryRequest
}

func (e *Engine) planDelivery(req *turn.TurnRequest, events []store.RuntimeEvent) (plannedDelivery, error) {
	if req == nil {
		return plannedDelivery{}, invalidArgument("turn request must not be nil")
	}
	adapter, ok := e.channelAdapterFor(req)
	if !ok {
		if req.Origin == turn.OriginTerminal || req.Origin == turn.OriginInternal || req.Origin == turn.OriginWebhook || req.Origin == turn.OriginHTTP || len(req.ReplyContext) == 0 {
			return plannedDelivery{}, codedError(turn.ErrorCodeDeliverySkipped, "turn has no channel destination", nil)
		}
		return plannedDelivery{}, codedError(turn.ErrorCodeDeliveryFailed, "owning channel adapter is not configured", nil)
	}
	parts := deliveryParts(extractDeliveryTexts(events))
	if len(parts) == 0 {
		return plannedDelivery{}, codedError(turn.ErrorCodeDeliverySkipped, "turn completed with no model text", nil)
	}
	return plannedDelivery{adapter: adapter, request: &runtimechannelhost.DeliveryRequest{
		EffectID:       req.TurnID,
		Channel:        string(req.Origin),
		ConversationID: req.SessionID,
		ReplyContext:   req.ReplyContext,
		Parts:          parts,
		IdempotencyKey: req.TurnID,
		Deadline:       req.Deadline,
	}}, nil
}

func (e *Engine) deliverCompletedTurn(ctx context.Context, req *turn.TurnRequest, events []store.RuntimeEvent) error {
	if req == nil {
		return invalidArgument("turn request must not be nil")
	}
	if e.hasDelivered(req.TurnID) {
		return nil
	}
	planned, err := e.planDelivery(req, events)
	if err != nil {
		if code, ok := turn.CodeOf(err); ok && code == turn.ErrorCodeDeliverySkipped {
			e.markDelivered(req.TurnID)
		}
		return err
	}
	if !e.markDelivered(req.TurnID) {
		return nil
	}
	if _, err := planned.adapter.Deliver(ctx, planned.request); err != nil {
		e.unmarkDelivered(req.TurnID)
		return codedError(turn.ErrorCodeDeliveryFailed, "channel delivery failed", err)
	}
	return nil
}

type deliveryOutcome string

const (
	deliverySent    deliveryOutcome = "sent"
	deliverySkipped deliveryOutcome = "skipped"
	deliveryFailed  deliveryOutcome = "failed"
)

type deliveryRecord struct {
	Outcome deliveryOutcome `json:"outcome"`
	Code    turn.ErrorCode  `json:"code,omitempty"`
	Detail  string          `json:"detail,omitempty"`
}

type completedTurn struct {
	req    *turn.TurnRequest
	events []store.RuntimeEvent
}

func (e *Engine) deliverOnce(ctx context.Context, done *completedTurn) deliveryRecord {
	planned, err := e.planDelivery(done.req, done.events)
	if err != nil {
		if code, ok := turn.CodeOf(err); ok && code == turn.ErrorCodeDeliverySkipped {
			e.logger.DebugContext(ctx, "turn delivery skipped", "error", err, "turn_id", done.req.TurnID)
			return deliveryRecord{Outcome: deliverySkipped}
		}
		return e.failedDelivery(ctx, done.req, err)
	}
	if _, err := planned.adapter.Deliver(ctx, planned.request); err != nil {
		return e.failedDelivery(ctx, done.req, err)
	}
	return deliveryRecord{Outcome: deliverySent}
}

func (e *Engine) failedDelivery(ctx context.Context, req *turn.TurnRequest, err error) deliveryRecord {
	code := turn.ErrorCodeDeliveryFailed
	if errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous) {
		code = turn.ErrorCodeDeliveryAmbiguous
	}
	e.logger.ErrorContext(ctx, "turn delivery failed", "error", err, "code", code, "turn_id", req.TurnID)
	return deliveryRecord{Outcome: deliveryFailed, Code: code, Detail: err.Error()}
}

func (r deliveryRecord) terminal(agentID string) (kind string, payload []byte) {
	if r.Outcome != deliveryFailed {
		return turn.EventKindTurnCompleted, completedPayload(agentID)
	}
	detail := "channel delivery failed"
	if r.Code == turn.ErrorCodeDeliveryAmbiguous {
		detail = "channel delivery outcome is unknown"
	}
	if r.Detail != "" {
		detail += ": " + r.Detail
	}
	return turn.EventKindTurnFailed, failedPayload(r.Code, detail, nil, agentID)
}

func DeliveryTexts(events []store.RuntimeEvent) []string {
	return extractDeliveryTexts(events)
}
