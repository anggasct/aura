package harness

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/sandbox"
)

type CompositionConfig struct {
	Runtime            runtime.AgentRuntime
	Broker             runtime.ToolBroker
	RequireSandbox     bool
	SandboxPrimitives  sandbox.Primitives
	Provider           Provider
	Descriptor         *Descriptor
	RequireProvider    bool
	NetworkDestination string
	NetworkResolver    egress.Resolver
}

type Composition struct {
	Runtime runtime.AgentRuntime
	Broker  runtime.ToolBroker
}

func NewComposition(ctx context.Context, cfg *CompositionConfig) (*Composition, error) {
	if ctx == nil {
		return nil, invalidArgument("composition context must not be nil")
	}
	if cfg == nil {
		return nil, invalidArgument("composition config must not be nil")
	}
	if cfg.Runtime == nil {
		return nil, invalidArgument("composition runtime must not be nil")
	}
	if cfg.Broker == nil {
		return nil, invalidArgument("composition tool broker must not be nil")
	}
	if cfg.RequireSandbox {
		if err := sandbox.Require(cfg.SandboxPrimitives); err != nil {
			return nil, codedError(ErrorCodeCapabilityUnavailable, "sandbox profile is unavailable", err)
		}
	}
	if cfg.RequireProvider && cfg.Provider == nil {
		return nil, codedError(ErrorCodeProviderUnavailable, "provider profile is unavailable", nil)
	}
	if cfg.Descriptor != nil {
		if err := validateDescriptor(cfg.Descriptor); err != nil {
			return nil, err
		}
		if cfg.Provider == nil {
			return nil, codedError(ErrorCodeProviderUnavailable, "descriptor has no provider", nil)
		}
		profile := cfg.Provider.Profile()
		if strings.TrimSpace(profile.Name) == "" || profile.Capability != cfg.Descriptor.Capability {
			return nil, codedError(ErrorCodeCapabilityUnavailable, "provider profile does not satisfy descriptor", nil)
		}
	}
	if cfg.NetworkDestination != "" {
		if _, err := egress.Validate(ctx, cfg.NetworkDestination, cfg.NetworkResolver); err != nil {
			return nil, codedError(ErrorCodeCapabilityUnavailable, "network profile is unavailable", err)
		}
	}
	return &Composition{Runtime: cfg.Runtime, Broker: cfg.Broker}, nil
}
