package runtimeengine

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

type SkillRenderer interface {
	RenderSkill(ctx context.Context, sessionID string, pin *turn.SkillPin) (caveat, body string, err error)
}

type skillActivationRender func(ctx context.Context, req *turn.TurnRequest) ([]byte, error)

func (e *Engine) renderSkillActivation(ctx context.Context, req *turn.TurnRequest) ([]byte, error) {
	if e.cfg.Skills == nil {
		return nil, codedError(turn.ErrorCodeSkillUnavailable, "skill activation is not configured", nil)
	}
	caveat, body, err := e.cfg.Skills.RenderSkill(ctx, req.SessionID, req.Skill)
	if err != nil {
		if code, ok := skills.CodeOf(err); ok && code == skills.ErrorCodeSkillUnavailable {
			e.logger.WarnContext(ctx, "skill activation unavailable", "component", "skills", "skill_id", req.Skill.SkillID, "turn_id", req.TurnID, "error", err)
			return nil, codedError(turn.ErrorCodeSkillUnavailable, "skill is not available", nil)
		}
		if code, ok := store.CodeOf(err); ok && (code == store.ErrorCodeStorageUnavailable || code == store.ErrorCodeStorageBusy) {
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
			if code, ok := turn.CodeOf(renderErr); ok && code == turn.ErrorCodeSkillUnavailable {
				return []byte("null"), nil
			}
			return nil, renderErr
		})
		if err != nil {
			if _, ok := turn.CodeOf(err); ok {
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

func (e *Engine) persistSkillActivation(ctx context.Context, req *turn.TurnRequest, render skillActivationRender, persist func(context.Context, *store.RuntimeEvent) error) error {
	if req.Skill == nil {
		return nil
	}
	payload, err := render(ctx, req)
	if err != nil {
		return err
	}
	return persist(ctx, &store.RuntimeEvent{
		Kind:    turn.EventKindSkillActivated,
		Author:  turn.AuthorSystem,
		Payload: payload,
	})
}
