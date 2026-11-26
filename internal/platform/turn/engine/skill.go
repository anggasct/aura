package runtimeengine

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
)

type SkillRenderer interface {
	RenderSkill(ctx context.Context, sessionID string, pin *turn.SkillPin) (caveat, body string, err error)
}

type skillActivationRender func(ctx context.Context, req *turn.TurnRequest) ([]byte, error)

func (e *Engine) renderSkillActivation(ctx context.Context, req *turn.TurnRequest) ([]byte, error) {
	if e.deps.Skills == nil {
		return nil, codedError(turn.ErrorCodeSkillUnavailable, "skill activation is not configured", nil)
	}
	caveat, body, err := e.deps.Skills.RenderSkill(ctx, req.SessionID, req.Skill)
	if err != nil {
		if fault.HasCode(err, turn.ErrorCodeSkillUnavailable) {
			e.logger.WarnContext(ctx, "skill activation unavailable", "component", "skills", "skill_id", req.Skill.SkillID, "turn_id", req.TurnID, "error", err)
			return nil, codedError(turn.ErrorCodeSkillUnavailable, "skill is not available", nil)
		}
		if _, ok := fault.CodeOf[sqlite.ErrorCode](err); ok {
			return nil, codedError(turn.ErrorCodeStorageUnavailable, "read the pinned skill activation", err)
		}
		return nil, codedError(turn.ErrorCodeRuntimeInternal, "render the pinned skill activation", err)
	}
	payload, err := json.Marshal(turn.SkillActivation{
		SkillID:       req.Skill.SkillID,
		Digest:        req.Skill.Digest,
		InvocationID:  req.Skill.InvocationID,
		PolicyVersion: req.Skill.PolicyVersion,
		TurnID:        req.TurnID,
		Caveat:        caveat,
		Body:          body,
	})
	if err != nil {
		return nil, invalidArgument("skill activation payload is not encodable")
	}
	return payload, nil
}

func journaledSkillActivation(scope *durable.TurnScope, render skillActivationRender) skillActivationRender {
	return func(ctx context.Context, req *turn.TurnRequest) ([]byte, error) {
		raw, err := scope.Invocation().RunAction(ctx, "skill-activation", func(ctx context.Context) ([]byte, error) {
			payload, renderErr := render(ctx, req)
			if renderErr == nil {
				return payload, nil
			}
			if fault.HasCode(renderErr, turn.ErrorCodeSkillUnavailable) {
				return []byte("null"), nil
			}
			return nil, renderErr
		})
		if err != nil {
			if _, ok := fault.CodeOf[turn.ErrorCode](err); ok {
				return nil, err
			}
			return nil, codedError(turn.ErrorCodeStorageUnavailable, "failed to journal the skill activation", err)
		}
		if string(raw) == "null" {
			return nil, codedError(turn.ErrorCodeSkillUnavailable, "skill is not available", nil)
		}
		return raw, nil
	}
}

func (e *Engine) persistSkillActivation(ctx context.Context, req *turn.TurnRequest, render skillActivationRender, persist func(context.Context, *turn.Event) error) error {
	if req.Skill == nil {
		return nil
	}
	payload, err := render(ctx, req)
	if err != nil {
		return err
	}
	return persist(ctx, &turn.Event{
		Kind:    turn.EventKindSkillActivated,
		Author:  turn.AuthorSystem,
		Payload: payload,
	})
}
