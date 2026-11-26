package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

type SessionAgentGetter interface {
	GetSessionAgent(ctx context.Context, sessionID string) (agentID string, found bool, err error)
}

type RegistryPersonas struct {
	Registry *Registry
}

func (p RegistryPersonas) Fingerprint(agentID string) (turn.PersonaFingerprint, bool) {
	definition, ok := p.Registry.Lookup(agentID)
	if !ok {
		return turn.PersonaFingerprint{}, false
	}
	fingerprint := definition.Fingerprint()
	return turn.PersonaFingerprint{
		AgentID:     fingerprint.AgentID,
		SoulSHA256:  fingerprint.SoulSHA256,
		ToolsSHA256: fingerprint.ToolsSHA256,
		ModelRoute:  fingerprint.ModelRoute,
	}, true
}

func (p RegistryPersonas) Knowledge(agentID string, class principal.Class) []string {
	definition, ok := p.Registry.Lookup(agentID)
	if !ok {
		return nil
	}
	return definition.KnowledgeFor(class)
}

func LogPersonas(ctx context.Context, logger *slog.Logger, registry *Registry) {
	definitions := registry.Definitions()
	for i := range definitions {
		definition := &definitions[i]
		fingerprint := definition.Fingerprint()
		logger.InfoContext(ctx, "persona registered", "component", "persona", "agent_id", definition.ID, "name", definition.Name,
			"soul_sha256", fingerprint.SoulSHA256, "tools_sha256", fingerprint.ToolsSHA256, "model_route", routeLabel(definition.ModelRoute))
	}
}

func ResolveChatPersona(ctx context.Context, registry *Registry, sessions SessionAgentGetter, sessionID, requested string) (string, error) {
	if requested != "" {
		if _, ok := registry.Lookup(requested); !ok {
			return "", fault.New(turn.ErrorCodePersonaNotFound, fmt.Sprintf("persona %q is not registered", requested))
		}
	}
	if sessionID != "" && sessions != nil {
		agentID, found, err := sessions.GetSessionAgent(ctx, sessionID)
		switch {
		case err != nil:
			return "", err
		case found && requested != "" && requested != agentID:
			return "", fault.New(turn.ErrorCodePersonaMismatch, fmt.Sprintf("session %s belongs to persona %q, not %q", sessionID, agentID, requested))
		case found:
			return agentID, nil
		}
	}
	if requested != "" {
		return requested, nil
	}
	return DefaultID, nil
}
