package personaeval

import (
	"context"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
)

type Gate struct {
	registry *auraagent.Registry
	results  Store
}

func NewGate(registry *auraagent.Registry, results Store) (*Gate, error) {
	if registry == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval gate registry must not be nil")
	}
	if results == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "persona eval gate store must not be nil")
	}
	return &Gate{registry: registry, results: results}, nil
}

func (g *Gate) Evaluated(ctx context.Context, persona string) (bool, error) {
	if g == nil || g.registry == nil || g.results == nil {
		return false, fault.New(ErrorCodeInvalidArgument, "persona eval gate is not initialized")
	}
	definition, ok := g.registry.Lookup(persona)
	if !ok {
		return false, nil
	}
	digest := definition.EvalDigest()
	latest, found, err := g.results.Latest(ctx, persona, digest, LayerPolicy)
	if err != nil || !found || !latest.Passed {
		return false, err
	}
	behavior := definition.EvalBehavior
	if behavior == nil || (!behavior.Required && !behavior.EnforceOnRegression) {
		return true, nil
	}
	scored, found, err := g.results.Latest(ctx, persona, digest, LayerBehavior)
	if err != nil {
		return false, err
	}
	if !found {
		return !behavior.Required, nil
	}
	return scored.Passed, nil
}
