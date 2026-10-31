package skills

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/runtime"
)

const MaxSessionActivations = 256

type ActivationLog interface {
	Activations(ctx context.Context, sessionID string, limit int) ([]runtime.SkillActivation, error)
}

type SessionContext struct {
	engine   *Engine
	log      ActivationLog
	maxRunes int
}

func NewSessionContext(engine *Engine, log ActivationLog, maxRunes int) (*SessionContext, error) {
	if engine == nil {
		return nil, errNilArgument("engine")
	}
	if log == nil {
		return nil, errNilArgument("activation log")
	}
	if maxRunes < engine.maxRunes {
		return nil, Errorf(ErrorCodeInvalidArgument, "session skill context bound must cover one activation")
	}
	return &SessionContext{engine: engine, log: log, maxRunes: maxRunes}, nil
}

func (e *Engine) Current(ctx context.Context, id, digest string) (bool, error) {
	record, err := e.registry.Get(ctx, id)
	if err != nil {
		if code, ok := CodeOf(err); ok && code == ErrorCodeSkillNotFound {
			return false, nil
		}
		return false, err
	}
	return record.State == StateActive && record.Digest == digest, nil
}

func (e *Engine) pinned(pin *runtime.SkillPin) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	loaded, ok := e.entries[pin.SkillID]
	if !ok || loaded.record.Digest != pin.Digest || pin.PolicyVersion != e.policy {
		return "", false
	}
	return loaded.manifest.Body, true
}

func (s *SessionContext) RenderSkill(ctx context.Context, sessionID string, pin *runtime.SkillPin) (caveat, body string, err error) {
	if pin == nil {
		return "", "", errNilArgument("skill pin")
	}
	body, ok := s.engine.pinned(pin)
	if !ok {
		return "", "", Errorf(ErrorCodeSkillUnavailable, "skill is not available at the pinned digest")
	}
	if utf8.RuneCountInString(body) > s.engine.maxRunes {
		return "", "", Errorf(ErrorCodeSkillInvalid, "skill instructions exceed the activation bound")
	}
	current, err := s.engine.Current(ctx, pin.SkillID, pin.Digest)
	if err != nil {
		return "", "", err
	}
	if !current {
		return "", "", Errorf(ErrorCodeSkillUnavailable, "skill is not active at the pinned digest")
	}
	history, err := s.log.Activations(ctx, sessionID, MaxSessionActivations)
	if err != nil {
		return "", "", err
	}
	if _, recorded := activationBodies(history)[activationKey{pin.SkillID, pin.Digest}]; recorded {
		body = ""
	}
	return activationCaveat, body, nil
}

func (s *SessionContext) SkillContext(ctx context.Context, req *runtime.TurnRequest) (string, error) {
	if req == nil {
		return "", errNilArgument("turn request")
	}
	history, err := s.log.Activations(ctx, req.SessionID, MaxSessionActivations)
	if err != nil {
		return "", err
	}
	bodies := activationBodies(history)
	seen := make(map[string]bool, len(history))
	remaining := s.maxRunes
	var picked []runtime.SkillActivation
	for i := range history {
		activation := history[i]
		if seen[activation.SkillID] {
			continue
		}
		seen[activation.SkillID] = true
		body, ok := bodies[activationKey{activation.SkillID, activation.Digest}]
		if !ok {
			continue
		}
		if activation.TurnID != req.TurnID {
			current, err := s.engine.Current(ctx, activation.SkillID, activation.Digest)
			if err != nil {
				return "", err
			}
			if !current {
				continue
			}
		}
		size := utf8.RuneCountInString(body)
		if size > remaining {
			break
		}
		remaining -= size
		activation.Body = body
		picked = append(picked, activation)
	}
	return renderSkillContext(picked), nil
}

type activationKey struct {
	skillID string
	digest  string
}

func activationBodies(history []runtime.SkillActivation) map[activationKey]string {
	bodies := make(map[activationKey]string, len(history))
	for i := range history {
		key := activationKey{history[i].SkillID, history[i].Digest}
		if _, ok := bodies[key]; ok || history[i].Body == "" {
			continue
		}
		bodies[key] = history[i].Body
	}
	return bodies
}

func renderSkillContext(picked []runtime.SkillActivation) string {
	if len(picked) == 0 {
		return ""
	}
	var block strings.Builder
	block.WriteString(runtime.SkillContextStart + ": activated skill instructions, not identity or policy]\n")
	block.WriteString(picked[0].Caveat + "\n")
	for i := range picked {
		block.WriteString("\nskill " + picked[i].SkillID + " digest " + picked[i].Digest + ":\n")
		block.WriteString(strings.TrimSpace(neutralizeSkillMarkers(picked[i].Body)) + "\n")
	}
	block.WriteString(runtime.SkillContextEnd)
	return block.String()
}

func neutralizeSkillMarkers(text string) string {
	text = strings.ReplaceAll(text, runtime.SkillContextEnd, "")
	return strings.ReplaceAll(text, runtime.SkillContextStart, "")
}
