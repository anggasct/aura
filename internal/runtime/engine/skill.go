package runtimeengine

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type SkillRenderer interface {
	RenderSkill(ctx context.Context, sessionID string, pin *runtime.SkillPin) (caveat, body string, err error)
}

type skillActivationRender func(ctx context.Context, req *runtime.TurnRequest) ([]byte, error)

func (e *Engine) renderSkillActivation(ctx context.Context, req *runtime.TurnRequest) ([]byte, error) {
	if e.cfg.Skills == nil {
		return nil, codedError(runtime.ErrorCodeSkillUnavailable, "skill activation is not configured", nil)
	}
	caveat, body, err := e.cfg.Skills.RenderSkill(ctx, req.SessionID, req.Skill)
	if err != nil {
		e.logger.WarnContext(ctx, "skill activation unavailable", "component", "skills", "skill_id", req.Skill.SkillID, "turn_id", req.TurnID, "error", err)
		return nil, codedError(runtime.ErrorCodeSkillUnavailable, "skill is not available", nil)
	}
	payload, err := json.Marshal(runtime.SkillActivation{
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
	return func(ctx context.Context, req *runtime.TurnRequest) ([]byte, error) {
		raw, err := scope.Invocation().RunAction(ctx, "skill-activation", func(ctx context.Context) ([]byte, error) {
			return journaledPayload(render(ctx, req)), nil
		})
		if err != nil {
			return nil, codedError(runtime.ErrorCodeStorageUnavailable, "failed to journal the skill activation", err)
		}
		if string(raw) == "null" {
			return nil, codedError(runtime.ErrorCodeSkillUnavailable, "skill is not available", nil)
		}
		return raw, nil
	}
}

func journaledPayload(payload []byte, err error) []byte {
	if err != nil {
		return []byte("null")
	}
	return payload
}

func (e *Engine) persistSkillActivation(ctx context.Context, req *runtime.TurnRequest, render skillActivationRender, persist func(context.Context, *store.RuntimeEvent) error) error {
	if req.Skill == nil {
		return nil
	}
	payload, err := render(ctx, req)
	if err != nil {
		return err
	}
	return persist(ctx, &store.RuntimeEvent{
		Kind:    runtime.EventKindSkillActivated,
		Author:  runtime.AuthorSystem,
		Payload: payload,
	})
}
