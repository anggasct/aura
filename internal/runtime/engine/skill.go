package runtimeengine

import (
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/skills"
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
		if code, ok := skills.CodeOf(err); ok && code == skills.ErrorCodeSkillUnavailable {
			e.logger.WarnContext(ctx, "skill activation unavailable", "component", "skills", "skill_id", req.Skill.SkillID, "turn_id", req.TurnID, "error", err)
			return nil, codedError(runtime.ErrorCodeSkillUnavailable, "skill is not available", nil)
		}
		if code, ok := store.CodeOf(err); ok && (code == store.ErrorCodeStorageUnavailable || code == store.ErrorCodeStorageBusy) {
			return nil, codedError(runtime.ErrorCodeStorageUnavailable, "read the pinned skill activation", err)
		}
		return nil, codedError(runtime.ErrorCodeRuntimeInternal, "render the pinned skill activation", err)
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
			payload, renderErr := render(ctx, req)
			if renderErr == nil {
				return payload, nil
			}
			if code, ok := runtime.CodeOf(renderErr); ok && code == runtime.ErrorCodeSkillUnavailable {
				return []byte("null"), nil
			}
			return nil, renderErr
		})
		if err != nil {
			if _, ok := runtime.CodeOf(err); ok {
				return nil, err
			}
			return nil, codedError(runtime.ErrorCodeStorageUnavailable, "failed to journal the skill activation", err)
		}
		if string(raw) == "null" {
			return nil, codedError(runtime.ErrorCodeSkillUnavailable, "skill is not available", nil)
		}
		return raw, nil
	}
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
