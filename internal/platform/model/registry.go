package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/usage"
)

type RegistryOptions struct {
	Logger     *slog.Logger
	Models     config.Models
	Routes     map[string]config.ModelRoute
	Checkpoint CircuitCheckpointStore
	Prices     *usage.PriceRegistry
	Vision     VisionWiring
}

type registryEntry struct {
	shared adkmodel.LLM
	build  func() (adkmodel.LLM, error)
}

type Registry struct {
	entries map[string]registryEntry
}

func NewRegistry(ctx context.Context, opts *RegistryOptions) (*Registry, error) {
	if opts == nil {
		return nil, newError(ErrorCodeInvalidArgument, "", "", "registry options must not be nil")
	}
	logger, models, routes, checkpoint, vw := opts.Logger, opts.Models, opts.Routes, opts.Checkpoint, opts.Vision
	prices := opts.Prices
	if prices == nil {
		prices = usage.NewPriceRegistry()
	}
	for name := range models.Definitions {
		def := models.Definitions[name]
		if def.Capabilities.MicrosPerInputToken > 0 || def.Capabilities.MicrosPerOutputToken > 0 {
			if existing, _ := prices.At(name, "USD", time.Now()); existing == nil {
				_ = prices.Put(&usage.Price{
					ModelDefinitionID:    name,
					Currency:             "USD",
					MicrosPerInputToken:  def.Capabilities.MicrosPerInputToken,
					MicrosPerOutputToken: def.Capabilities.MicrosPerOutputToken,
					EffectiveFrom:        time.Unix(0, 0),
					MaxReservationRate:   100,
					Source:               "config",
				})
			}
		}
	}

	adapters, circuits, err := BuildComponents(logger, models, routes, checkpoint, prices, vw)
	if err != nil {
		return nil, err
	}
	if circuits != nil && checkpoint != nil {
		if err := circuits.LoadCheckpoints(ctx); err != nil {
			return nil, err
		}
	}

	routeAdapters := make(map[string]adkmodel.LLM, len(routes))
	for name, route := range routes {
		fb := NewFallbackAdapter(name, route, models.Definitions, circuits, MapAdapterResolver(adapters)).WithLogger(logger).WithPrices(prices).WithVisionPolicy(vw.Policy).WithProviders(models.Providers)
		routeAdapters[name] = fb
	}

	timeout := time.Duration(models.RequestTimeout)
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	idleTimeout := time.Duration(models.StreamingIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}

	entries := make(map[string]registryEntry, len(models.Definitions)+len(routeAdapters))
	for _, role := range slices.Sorted(maps.Keys(models.Definitions)) {
		spec := models.Definitions[role]
		_, configured, err := newAdapter(logger, role, &spec, models.Providers, timeout, idleTimeout, vw)
		if err != nil {
			return nil, err
		}
		if !configured {
			continue
		}
		if _, taken := entries[spec.Model]; taken {
			return nil, newError(ErrorCodeProtocolInvalid, role, "", fmt.Sprintf("model %q is already registered", spec.Model))
		}
		definitionID := role
		entries[spec.Model] = registryEntry{build: func() (adkmodel.LLM, error) {
			adapter, _, err := newAdapter(logger, definitionID, &spec, models.Providers, timeout, idleTimeout, vw)
			return adapter, err
		}}
	}
	for _, name := range slices.Sorted(maps.Keys(routeAdapters)) {
		if _, taken := entries[name]; taken {
			return nil, newError(ErrorCodeProtocolInvalid, name, "", fmt.Sprintf("model %q is already registered", models.Definitions[name].Model))
		}
		entries[name] = registryEntry{shared: routeAdapters[name]}
	}
	return &Registry{entries: entries}, nil
}

func (r *Registry) LLM(_ context.Context, name string) (adkmodel.LLM, error) {
	entry, ok := r.entries[name]
	if !ok {
		return nil, newError(ErrorCodeNotFound, name, "", fmt.Sprintf("model %q is not registered", name))
	}
	if entry.build == nil {
		return entry.shared, nil
	}
	return entry.build()
}

type Set map[string]adkmodel.LLM

func (s Set) LLM(_ context.Context, name string) (adkmodel.LLM, error) {
	llm, ok := s[name]
	if !ok {
		return nil, newError(ErrorCodeNotFound, name, "", fmt.Sprintf("model %q is not registered", name))
	}
	return llm, nil
}

func RouteModelName(cfg *config.Config, defaultModel string) (func(route string) (string, error), error) {
	resolver := func(route string) (string, error) {
		if r, ok := cfg.ModelRoutes[route]; ok && len(r.Candidates) > 0 {
			return route, nil
		}
		if route == "" {
			if cfg.Models.Definitions["primary"].Model == "" {
				return "", errors.New("no default model is configured")
			}
			return defaultModel, nil
		}
		if definition, ok := cfg.Models.Definitions[route]; ok && definition.Model != "" {
			return definition.Model, nil
		}
		return "", fmt.Errorf("unknown model route %q", route)
	}
	if defaultModel == "" {
		defaultModel = cfg.Models.Definitions["primary"].Model
		if defaultModel == "" {
			return nil, errors.New("model: no primary model is configured")
		}
	}
	return resolver, nil
}
