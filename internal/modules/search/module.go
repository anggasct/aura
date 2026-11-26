package search

import (
	"fmt"
	"time"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

const (
	defaultTimeout      = 30 * time.Second
	defaultMaxBodyBytes = 8 * 1024 * 1024
)

type Deps struct {
	Config       *Config
	Timeout      time.Duration
	MaxBodyBytes int64
	Resolver     egress.Resolver
}

type Module struct {
	adapter toolbroker.Adapter
	config  Config
}

func New(deps *Deps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "search module dependencies must not be nil")
	}
	cfg := Default()
	if deps.Config != nil {
		cfg = *deps.Config
	}
	timeout := deps.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	maxBodyBytes := deps.MaxBodyBytes
	if maxBodyBytes == 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	options := &Options{
		Provider:      cfg.Provider,
		CredentialRef: cfg.CredentialRef,
		Timeout:       timeout,
		MaxResults:    cfg.MaxResults,
		MaxBodyBytes:  maxBodyBytes,
		Resolver:      deps.Resolver,
	}
	if cfg.Endpoint != "" {
		endpoint, err := cfg.SearchEndpoint()
		if err != nil {
			return nil, fmt.Errorf("web search endpoint: %w", err)
		}
		options.Endpoint = endpoint.URL()
		if endpoint.Loopback {
			options.LoopbackAuthority = endpoint.Authority()
		}
	}
	adapter, err := NewAdapter(options)
	if err != nil {
		return nil, err
	}
	return &Module{adapter: adapter, config: cfg}, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || m.adapter == nil {
		return nil
	}
	return []toolbroker.ToolRegistration{{
		Definition: ToolDefinition(),
		Adapter:    m.adapter,
		Rule:       ToolRule(),
	}}
}

func (m *Module) Adapter() toolbroker.Adapter {
	if m == nil {
		return nil
	}
	return m.adapter
}

func (m *Module) Secrets() []string {
	if m == nil {
		return nil
	}
	return m.config.ConfiguredSecrets()
}
