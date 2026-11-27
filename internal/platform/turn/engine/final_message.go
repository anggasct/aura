package runtimeengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

const finalMessageAction = "final-message"

type finalKind string

const (
	finalAnswer      finalKind = "answer"
	finalTurnEmpty   finalKind = "turn_empty"
	finalTurnTimeout finalKind = "turn_timeout"
	finalTurnFailed  finalKind = "turn_failed"
	finalNone        finalKind = "none"
)

type finalMessage struct {
	Kind   finalKind      `json:"kind"`
	Code   turn.ErrorCode `json:"code,omitempty"`
	Detail string         `json:"detail,omitempty"`
	Parts  []string       `json:"parts"`
}

type turnEnd struct {
	cancelled bool
	code      turn.ErrorCode
	detail    string
}

func (e *Engine) endOf(ctxErr, execErr error) turnEnd {
	switch {
	case errors.Is(ctxErr, context.Canceled):
		return turnEnd{cancelled: true}
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return turnEnd{code: turn.ErrorCodeTurnDeadlineExceeded, detail: "turn deadline elapsed"}
	case execErr != nil:
		code, ok := fault.CodeOf[turn.ErrorCode](execErr)
		if !ok {
			code = turn.ErrorCodeRuntimeInternal
		}
		return turnEnd{code: code, detail: "turn execution failed: " + execErr.Error()}
	default:
		return turnEnd{}
	}
}

func (m *finalMessage) failed() bool {
	return m.Code != ""
}

func (m *finalMessage) valid() bool {
	switch m.Kind {
	case finalAnswer, finalTurnEmpty, finalTurnTimeout, finalTurnFailed, finalNone:
		return true
	default:
		return false
	}
}

func (m *finalMessage) terminal(agentID string, delivered deliveryRecord) (kind string, payload []byte) {
	if m.failed() {
		return turn.EventKindTurnFailed, failedPayload(m.Code, m.Detail, nil, agentID)
	}
	return delivered.terminal(agentID)
}

func (e *Engine) planFinalMessage(req *turn.TurnRequest, events []turn.Event, end turnEnd) finalMessage {
	final := finalMessage{Code: end.code, Detail: end.detail, Parts: []string{}}
	switch {
	case !hasChannelDestination(req):
		final.Kind = finalNone
	case end.code == turn.ErrorCodeTurnDeadlineExceeded:
		final.Kind = finalTurnTimeout
		final.Parts = []string{e.fallbackText(req, agent.MessageTurnTimeout)}
	case end.code != "":
		final.Kind = finalTurnFailed
		final.Parts = []string{e.fallbackText(req, agent.MessageTurnFailed)}
	default:
		e.planCompletedMessage(req, events, &final)
	}
	return final
}

func (e *Engine) planCompletedMessage(req *turn.TurnRequest, events []turn.Event, final *finalMessage) {
	switch texts := extractDeliveryTexts(events); {
	case len(texts) > 0:
		final.Kind = finalAnswer
		final.Parts = texts
	case e.repliedToRequester(events):
		final.Kind = finalNone
	default:
		final.Kind = finalTurnEmpty
		final.Parts = []string{e.fallbackText(req, agent.MessageTurnEmpty)}
	}
}

func (e *Engine) startFailureMessage(req *turn.TurnRequest, code turn.ErrorCode, detail string) finalMessage {
	return finalMessage{Kind: finalTurnFailed, Code: code, Detail: detail, Parts: []string{e.fallbackText(req, agent.MessageTurnFailed)}}
}

func (e *Engine) fallbackText(req *turn.TurnRequest, key string) string {
	if e.deps.Personas != nil {
		if text := e.deps.Personas.Message(e.agentID(req), key); strings.TrimSpace(text) != "" {
			return text
		}
	}
	return agent.PersonaMessageDefaults()[key]
}

func (e *Engine) repliedToRequester(events []turn.Event) bool {
	if e.deps.RepliesToRequester == nil {
		return false
	}
	for i := range events {
		for _, response := range functionResponses(&events[i]) {
			if e.deps.RepliesToRequester(response.Name) && !turn.IsToolError(response.Response) {
				return true
			}
		}
	}
	return false
}

func (e *Engine) journalFinalMessage(ctx context.Context, scope *durable.TurnScope, done *endedTurn, end turnEnd) (finalMessage, error) {
	if done == nil || done.req == nil {
		return finalMessage{}, invalidArgument("final message requires the ended turn")
	}
	raw, err := scope.Invocation().RunAction(ctx, finalMessageAction, func(context.Context) ([]byte, error) {
		return json.Marshal(e.planFinalMessage(done.req, done.events, end))
	})
	if err != nil {
		return finalMessage{}, codedError(turn.ErrorCodeStorageUnavailable, "failed to journal the final message", err)
	}
	var final finalMessage
	if err := json.Unmarshal(raw, &final); err != nil || !final.valid() {
		return finalMessage{}, codedError(turn.ErrorCodeStorageUnavailable, "failed to decode the final message", err)
	}
	return final, nil
}
